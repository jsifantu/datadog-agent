package api

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"io/ioutil"
	stdlog "log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinylib/msgp/msgp"

	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/info"
	"github.com/DataDog/datadog-agent/pkg/trace/metrics"
	"github.com/DataDog/datadog-agent/pkg/trace/metrics/timing"
	"github.com/DataDog/datadog-agent/pkg/trace/osutil"
	"github.com/DataDog/datadog-agent/pkg/trace/pb"
	"github.com/DataDog/datadog-agent/pkg/trace/sampler"
	"github.com/DataDog/datadog-agent/pkg/trace/watchdog"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	maxRequestBodyLength = 10 * 1024 * 1024
	tagTraceHandler      = "handler:traces"
	tagServiceHandler    = "handler:services"

	// headerTraceCount is the header client implementation should fill
	// with the number of traces contained in the payload.
	headerTraceCount = "X-Datadog-Trace-Count"
)

// Version is a dumb way to version our collector handlers
type Version string

const (
	// v01 DEPRECATED, FIXME[1.x]
	// Traces: JSON, slice of spans
	// Services: deprecated
	v01 Version = "v0.1"
	// v02 DEPRECATED, FIXME[1.x]
	// Traces: JSON, slice of traces
	// Services: deprecated
	v02 Version = "v0.2"
	// v03
	// Traces: msgpack/JSON (Content-Type) slice of traces
	// Services: deprecated
	v03 Version = "v0.3"
	// v04
	// Traces: msgpack/JSON (Content-Type) slice of traces + returns service sampling ratios
	// Services: deprecated
	v04 Version = "v0.4"
)

// HTTPReceiver is a collector that uses HTTP protocol and just holds
// a chan where the spans received are sent one by one
type HTTPReceiver struct {
	Stats       *info.ReceiverStats
	RateLimiter *rateLimiter
	Out         chan pb.Trace

	conf    *config.AgentConfig
	dynConf *sampler.DynamicConfig
	server  *http.Server

	maxRequestBodyLength int64
	debug                bool
	rateLimiterResponse  int // HTTP status code when refusing

	wg   sync.WaitGroup // waits for all requests to be processed
	exit chan struct{}
}

// NewHTTPReceiver returns a pointer to a new HTTPReceiver
func NewHTTPReceiver(
	conf *config.AgentConfig, dynConf *sampler.DynamicConfig, out chan pb.Trace) *HTTPReceiver {
	rateLimiterResponse := http.StatusOK
	if config.HasFeature("429") {
		rateLimiterResponse = http.StatusTooManyRequests
	}
	// use buffered channels so that handlers are not waiting on downstream processing
	return &HTTPReceiver{
		Stats:       info.NewReceiverStats(),
		RateLimiter: newRateLimiter(),
		Out:         out,

		conf:    conf,
		dynConf: dynConf,

		maxRequestBodyLength: maxRequestBodyLength,
		debug:                strings.ToLower(conf.LogLevel) == "debug",
		rateLimiterResponse:  rateLimiterResponse,

		exit: make(chan struct{}),
	}
}

// Start starts doing the HTTP server and is ready to receive traces
func (r *HTTPReceiver) Start() {
	mux := http.NewServeMux()

	r.attachDebugHandlers(mux)

	mux.HandleFunc("/spans", r.httpHandleWithVersion(v01, r.handleTraces))
	mux.HandleFunc("/services", r.httpHandleWithVersion(v01, r.handleServices))
	mux.HandleFunc("/v0.1/spans", r.httpHandleWithVersion(v01, r.handleTraces))
	mux.HandleFunc("/v0.1/services", r.httpHandleWithVersion(v01, r.handleServices))
	mux.HandleFunc("/v0.2/traces", r.httpHandleWithVersion(v02, r.handleTraces))
	mux.HandleFunc("/v0.2/services", r.httpHandleWithVersion(v02, r.handleServices))
	mux.HandleFunc("/v0.3/traces", r.httpHandleWithVersion(v03, r.handleTraces))
	mux.HandleFunc("/v0.3/services", r.httpHandleWithVersion(v03, r.handleServices))
	mux.HandleFunc("/v0.4/traces", r.httpHandleWithVersion(v04, r.handleTraces))
	mux.HandleFunc("/v0.4/services", r.httpHandleWithVersion(v04, r.handleServices))

	timeout := 5 * time.Second
	if r.conf.ReceiverTimeout > 0 {
		timeout = time.Duration(r.conf.ReceiverTimeout) * time.Second
	}
	r.server = &http.Server{
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		ErrorLog:     stdlog.New(writableFunc(log.Error), "http.Server: ", 0),
		Handler:      mux,
	}

	addr := fmt.Sprintf("%s:%d", r.conf.ReceiverHost, r.conf.ReceiverPort)
	ln, err := r.listenTCP(addr)
	if err != nil {
		killProcess("Error creating tcp listener: %v", err)
	}
	go func() {
		defer watchdog.LogOnPanic()
		r.server.Serve(ln)
	}()
	log.Infof("Listening for traces at http://%s", addr)

	if path := r.conf.ReceiverSocket; path != "" {
		ln, err := r.listenUnix(path)
		if err != nil {
			killProcess("Error creating UDS listener: %v", err)
		}
		go func() {
			defer watchdog.LogOnPanic()
			r.server.Serve(ln)
		}()
		log.Infof("Listening for traces at unix://%s", path)
	}

	go r.RateLimiter.Run()

	go func() {
		defer watchdog.LogOnPanic()
		r.loop()
	}()
}

func (r *HTTPReceiver) attachDebugHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.HandleFunc("/debug/blockrate", func(w http.ResponseWriter, r *http.Request) {
		// this endpoint calls runtime.SetBlockProfileRate(v), where v is an optional
		// query string parameter defaulting to 10000 (1 sample per 10μs blocked).
		rate := 10000
		v := r.URL.Query().Get("v")
		if v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				http.Error(w, "v must be an integer", http.StatusBadRequest)
				return
			}
			rate = n
		}
		runtime.SetBlockProfileRate(rate)
		w.Write([]byte(fmt.Sprintf("Block profile rate set to %d. It will automatically be disabled again after calling /debug/pprof/block\n", rate)))
	})

	mux.HandleFunc("/debug/pprof/block", func(w http.ResponseWriter, r *http.Request) {
		// serve the block profile and reset the rate to 0.
		pprof.Handler("block").ServeHTTP(w, r)
		runtime.SetBlockProfileRate(0)
	})

	mux.Handle("/debug/vars", expvar.Handler())
}

// listenUnix returns a net.Listener listening on the given "unix" socket path.
func (r *HTTPReceiver) listenUnix(path string) (net.Listener, error) {
	fi, err := os.Stat(path)
	if err == nil {
		// already exists
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("cannot reuse %q; not a unix socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("unable to remove stale socket: %v", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0722); err != nil {
		return nil, fmt.Errorf("error setting socket permissions: %v", err)
	}
	return ln, err
}

// listenTCP creates a new net.Listener on the provided TCP address.
func (r *HTTPReceiver) listenTCP(addr string) (net.Listener, error) {
	tcpln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := newRateLimitedListener(tcpln, r.conf.ConnectionLimit)
	go func() {
		defer watchdog.LogOnPanic()
		ln.Refresh(r.conf.ConnectionLimit)
	}()
	return ln, err
}

// Stop stops the receiver and shuts down the HTTP server.
func (r *HTTPReceiver) Stop() error {
	r.exit <- struct{}{}
	<-r.exit

	r.RateLimiter.Stop()

	expiry := time.Now().Add(5 * time.Second) // give it 5 seconds
	ctx, cancel := context.WithDeadline(context.Background(), expiry)
	defer cancel()
	if err := r.server.Shutdown(ctx); err != nil {
		return err
	}
	r.wg.Wait()
	close(r.Out)
	return nil
}

func (r *HTTPReceiver) httpHandle(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		req.Body = NewLimitedReader(req.Body, r.maxRequestBodyLength)
		defer req.Body.Close()

		fn(w, req)
	}
}

func (r *HTTPReceiver) httpHandleWithVersion(v Version, f func(Version, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return r.httpHandle(func(w http.ResponseWriter, req *http.Request) {
		mediaType := getMediaType(req)
		if mediaType == "application/msgpack" && (v == v01 || v == v02) {
			// msgpack is only supported for versions >= 0.3
			httpFormatError(w, v, fmt.Errorf("unsupported media type: %q", mediaType))
			return
		}

		f(v, w, req)
	})
}

func traceCount(req *http.Request) int64 {
	str := req.Header.Get(headerTraceCount)
	if str == "" {
		return 0
	}
	n, err := strconv.Atoi(str)
	if err != nil {
		log.Errorf("Error parsing %q HTTP header: %s", headerTraceCount, err)
	}
	return int64(n)
}

func (r *HTTPReceiver) tagStats(req *http.Request) *info.TagStats {
	return r.Stats.GetTagStats(info.Tags{
		Lang:          req.Header.Get("Datadog-Meta-Lang"),
		LangVersion:   req.Header.Get("Datadog-Meta-Lang-Version"),
		Interpreter:   req.Header.Get("Datadog-Meta-Lang-Interpreter"),
		TracerVersion: req.Header.Get("Datadog-Meta-Tracer-Version"),
	})
}

func (r *HTTPReceiver) decodeTraces(v Version, req *http.Request) (pb.Traces, error) {
	if v == v01 {
		var spans []pb.Span
		if err := json.NewDecoder(req.Body).Decode(&spans); err != nil {
			return nil, err
		}
		return tracesFromSpans(spans), nil
	}
	var traces pb.Traces
	if err := decodeRequest(req, &traces); err != nil {
		return nil, err
	}
	return traces, nil
}

func (r *HTTPReceiver) replyOK(v Version, w http.ResponseWriter) {
	switch v {
	case v01, v02, v03:
		httpOK(w)
	case v04:
		httpRateByService(w, r.dynConf)
	}
}

// handleTraces knows how to handle a bunch of traces
func (r *HTTPReceiver) handleTraces(v Version, w http.ResponseWriter, req *http.Request) {
	traceCount := traceCount(req)
	if !r.RateLimiter.Permits(traceCount) {
		io.Copy(ioutil.Discard, req.Body)
		w.WriteHeader(r.rateLimiterResponse)
		r.replyOK(v, w)
		metrics.Count("datadog.trace_agent.receiver.payload_refused", 1, nil, 1)
		return
	}

	ts := r.tagStats(req)
	traces, err := r.decodeTraces(v, req)
	if err != nil {
		httpDecodingError(err, []string{tagTraceHandler, fmt.Sprintf("v:%s", v)}, w)
		atomic.AddInt64(&ts.TracesDropped.DecodingError, traceCount)
		log.Errorf("Cannot decode %s traces payload: %v", v, err)
		return
	}
	r.replyOK(v, w)

	atomic.AddInt64(&ts.TracesReceived, int64(len(traces)))
	atomic.AddInt64(&ts.TracesBytes, int64(req.Body.(*LimitedReader).Count))
	atomic.AddInt64(&ts.PayloadAccepted, 1)

	r.wg.Add(1)
	go func() {
		defer func() {
			r.wg.Done()
			watchdog.LogOnPanic()
		}()
		r.processTraces(ts, traces)
	}()
}

func (r *HTTPReceiver) processTraces(ts *info.TagStats, traces pb.Traces) {
	defer timing.Since("datadog.trace_agent.internal.normalize_ms", time.Now())
	for _, trace := range traces {
		spans := len(trace)

		atomic.AddInt64(&ts.SpansReceived, int64(spans))

		err := normalizeTrace(ts, trace)
		if err != nil {
			log.Debug("Dropping invalid trace: %s", err)
			atomic.AddInt64(&ts.SpansDropped, int64(spans))
			continue
		}

		r.Out <- trace
	}
}

// handleServices handle a request with a list of several services
func (r *HTTPReceiver) handleServices(v Version, w http.ResponseWriter, req *http.Request) {
	httpOK(w)

	// Do nothing, services are no longer being sent to Datadog as of July 2019
	// and are now automatically extracted from traces.
}

// loop periodically submits stats about the receiver to statsd
func (r *HTTPReceiver) loop() {
	defer close(r.exit)

	var lastLog time.Time
	accStats := info.NewReceiverStats()

	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	tw := time.NewTicker(r.conf.WatchdogInterval)
	defer tw.Stop()

	for {
		select {
		case <-r.exit:
			return
		case now := <-tw.C:
			r.watchdog(now)
		case now := <-t.C:
			metrics.Gauge("datadog.trace_agent.heartbeat", 1, nil, 1)
			metrics.Gauge("datadog.trace_agent.receiver.out_chan_fill", float64(len(r.Out))/float64(cap(r.Out)), nil, 1)

			// We update accStats with the new stats we collected
			accStats.Acc(r.Stats)

			// Publish the stats accumulated during the last flush
			r.Stats.Publish()

			// We reset the stats accumulated during the last 10s.
			r.Stats.Reset()

			if now.Sub(lastLog) >= time.Minute {
				// We expose the stats accumulated to expvar
				info.UpdateReceiverStats(accStats)

				accStats.LogStats()

				// We reset the stats accumulated during the last minute
				accStats.Reset()
				lastLog = now

				// Also publish rates by service (they are updated by receiver)
				rates := r.dynConf.RateByService.GetAll()
				info.UpdateRateByService(rates)
			}
		}
	}
}

// killProcess exits the process with the given msg; replaced in tests.
var killProcess = func(format string, a ...interface{}) { osutil.Exitf(format, a...) }

// watchdog checks the trace-agent's heap and CPU usage and updates the rate limiter using a correct
// sampling rate to maintain resource usage within set thresholds. These thresholds are defined by
// the configuration MaxMemory and MaxCPU. If these values are 0, all limits are disabled and the rate
// limiter will accept everything.
func (r *HTTPReceiver) watchdog(now time.Time) {
	wi := watchdog.Info{
		Mem: watchdog.Mem(),
		CPU: watchdog.CPU(now),
	}
	rateMem := 1.0
	if r.conf.MaxMemory > 0 {
		if current, allowed := float64(wi.Mem.Alloc), r.conf.MaxMemory*1.5; current > allowed {
			// This is a safety mechanism: if the agent is using more than 1.5x max. memory, there
			// is likely a leak somewhere; we'll kill the process to avoid polluting host memory.
			metrics.Count("datadog.trace_agent.receiver.oom_kill", 1, nil, 1)
			metrics.Flush()
			log.Criticalf("Killing process. Memory threshold exceeded: %.2fM / %.2fM", current/1024/1024, allowed/1024/1024)
			killProcess("OOM")
		}
		rateMem = computeRateLimitingRate(r.conf.MaxMemory, float64(wi.Mem.Alloc), r.RateLimiter.RealRate())
		if rateMem < 1 {
			log.Warnf("Memory threshold exceeded (apm_config.max_memory: %.0f bytes): %d", r.conf.MaxMemory, wi.Mem.Alloc)
		}
	}
	rateCPU := 1.0
	if r.conf.MaxCPU > 0 {
		rateCPU = computeRateLimitingRate(r.conf.MaxCPU, wi.CPU.UserAvg, r.RateLimiter.RealRate())
		if rateCPU < 1 {
			log.Warnf("CPU threshold exceeded (apm_config.max_cpu_percent: %.0f): %.0f", r.conf.MaxCPU*100, wi.CPU.UserAvg)
		}
	}

	r.RateLimiter.SetTargetRate(math.Min(rateCPU, rateMem))

	stats := r.RateLimiter.Stats()

	info.UpdateRateLimiter(*stats)
	info.UpdateWatchdogInfo(wi)

	metrics.Gauge("datadog.trace_agent.heap_alloc", float64(wi.Mem.Alloc), nil, 1)
	metrics.Gauge("datadog.trace_agent.cpu_percent", wi.CPU.UserAvg*100, nil, 1)
	metrics.Gauge("datadog.trace_agent.receiver.ratelimit", stats.TargetRate, nil, 1)
}

// Languages returns the list of the languages used in the traces the agent receives.
func (r *HTTPReceiver) Languages() string {
	// We need to use this map because we can have several tags for a same language.
	langs := make(map[string]bool)
	str := []string{}

	r.Stats.RLock()
	for tags := range r.Stats.Stats {
		if _, ok := langs[tags.Lang]; !ok {
			str = append(str, tags.Lang)
			langs[tags.Lang] = true
		}
	}
	r.Stats.RUnlock()

	sort.Strings(str)
	return strings.Join(str, "|")
}

func decodeRequest(req *http.Request, dest msgp.Decodable) error {
	switch mediaType := getMediaType(req); mediaType {
	case "application/msgpack":
		return msgp.Decode(req.Body, dest)
	case "application/json":
		fallthrough
	case "text/json":
		fallthrough
	case "":
		return json.NewDecoder(req.Body).Decode(dest)
	default:
		// do our best
		if err1 := json.NewDecoder(req.Body).Decode(dest); err1 != nil {
			if err2 := msgp.Decode(req.Body, dest); err2 != nil {
				return fmt.Errorf("could not decode JSON (%q), nor Msgpack (%q)", err1, err2)
			}
		}
		return nil
	}
}

func tracesFromSpans(spans []pb.Span) pb.Traces {
	traces := pb.Traces{}
	byID := make(map[uint64][]*pb.Span)
	for _, s := range spans {
		byID[s.TraceID] = append(byID[s.TraceID], &s)
	}
	for _, t := range byID {
		traces = append(traces, t)
	}

	return traces
}

// getMediaType attempts to return the media type from the Content-Type MIME header. If it fails
// it returns the default media type "application/json".
func getMediaType(req *http.Request) string {
	mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		log.Debugf(`Error parsing media type: %v, assuming "application/json"`, err)
		return "application/json"
	}
	return mt
}

// writableFunc implements io.Writer over a function. Anything written will be
// forwarded to the function as one string argument.
type writableFunc func(v ...interface{}) error

// Write implements io.Writer.
func (fn writableFunc) Write(p []byte) (n int, err error) {
	if err = fn(string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}
