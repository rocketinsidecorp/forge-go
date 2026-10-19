package telemetry

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

// Metric and label names of the HTTP instrumentation.
const (
	MetricHTTPServerDuration = "http.server.request.duration"
	MetricHTTPServerActive   = "http.server.active_requests"
	MetricHTTPClientDuration = "http.client.request.duration"
)

const httpScope = ScopeName + "/httpserver"

// ServerMiddleware returns middleware that traces each request with a
// server span and records http.server.request.duration (labels: method,
// route pattern, status class) and http.server.active_requests (label:
// method). It installs a web.RouteInfo so the router can report the matched
// pattern, adds trace_id and span_id to the request logger (web.Logger), and
// continues incoming traces when TrustIncoming is set.
//
// Recorded span attributes: http.request.method, http.route,
// http.response.status_code, url.scheme, network.protocol.version and, for
// 5xx responses, error.type. The raw path, query string, headers, client
// address and bodies are never recorded. Methods outside the standard set
// are recorded as _OTHER, and requests no route matched have an empty route,
// so request input cannot create new series.
//
// A nil Telemetry returns nil: callers install no middleware at all.
func (t *Telemetry) ServerMiddleware() (func(http.Handler) http.Handler, error) {
	if t == nil {
		return nil, nil
	}
	duration, err := t.metrics.Histogram(MetricHTTPServerDuration, "Duration of HTTP server requests.", "s", DurationBuckets,
		"http.request.method", "http.route", "http.response.status_class")
	if err != nil {
		return nil, err
	}
	active, err := t.metrics.Gauge(MetricHTTPServerActive, "Number of HTTP server requests in progress.", "{request}", "http.request.method")
	if err != nil {
		return nil, err
	}
	instrumentation := &serverInstrumentation{tracer: t.Tracer(httpScope), trust: t.trustIncoming, duration: duration, active: active}
	return instrumentation.wrap, nil
}

type serverInstrumentation struct {
	tracer   *Tracer
	trust    bool
	duration *Histogram
	active   *Gauge
}

func (s *serverInstrumentation) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		method := NormalizeMethod(r.Method)
		ctx := r.Context()
		if s.trust {
			ctx = Extract(ctx, r.Header)
		}
		route := &web.RouteInfo{}
		ctx = web.WithRouteInfo(ctx, route)
		ctx, span := s.tracer.Start(ctx, method, SpanKindServer)
		if span.IsRecording() {
			span.SetAttributes(
				String("http.request.method", method),
				String("url.scheme", scheme(r)),
				String("network.protocol.version", protocolVersion(r)),
			)
		}
		if sc := span.SpanContext(); sc.IsValid() {
			ctx = web.WithLogger(ctx, web.Logger(ctx).With(LogTraceIDKey, sc.TraceID.String(), LogSpanIDKey, sc.SpanID.String()))
		}
		s.active.Add(1, method)
		recorder := &statusRecorder{ResponseWriter: w}
		defer func() {
			s.active.Add(-1, method)
			status := recorder.statusCode()
			recovered := recover()
			if recovered != nil {
				status = http.StatusInternalServerError
			}
			s.finish(span, start, method, route.Pattern, status)
			if recovered != nil {
				panic(recovered)
			}
		}()
		next.ServeHTTP(recorder, r.WithContext(ctx))
	})
}

func (s *serverInstrumentation) finish(span *Span, start time.Time, method, pattern string, status int) {
	route := RoutePath(pattern)
	if span.IsRecording() {
		if route != "" {
			span.SetName(method + " " + route)
			span.SetAttributes(String("http.route", route))
		}
		span.SetAttributes(Int("http.response.status_code", status))
		if status >= http.StatusInternalServerError {
			code := strconv.Itoa(status)
			span.SetAttributes(String("error.type", code))
			span.SetStatus(StatusError, code)
		}
	}
	span.End()
	s.duration.Record(time.Since(start).Seconds(), method, route, StatusClass(status))
}

// RoutePath returns the path of a ServeMux pattern: "GET /users/{id}" gives
// "/users/{id}".
func RoutePath(pattern string) string {
	if _, path, found := strings.Cut(pattern, " "); found {
		return strings.TrimLeft(path, " \t")
	}
	return pattern
}

var statusClasses = [...]string{"1xx", "2xx", "3xx", "4xx", "5xx"}

// StatusClass returns "1xx" to "5xx", or "other" for an invalid status.
func StatusClass(status int) string {
	if status >= 100 && status < 600 {
		return statusClasses[status/100-1]
	}
	return "other"
}

// NormalizeMethod returns method if it is a standard HTTP method and
// "_OTHER" otherwise, as OpenTelemetry requires, so a client cannot create
// metric series with invented methods.
func NormalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, "QUERY":
		return method
	}
	return "_OTHER"
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func protocolVersion(r *http.Request) string {
	switch {
	case r.ProtoMajor == 1 && r.ProtoMinor == 1:
		return "1.1"
	case r.ProtoMajor == 2:
		return "2"
	case r.ProtoMajor == 1 && r.ProtoMinor == 0:
		return "1.0"
	case r.ProtoMajor == 3:
		return "3"
	}
	return strconv.Itoa(r.ProtoMajor) + "." + strconv.Itoa(r.ProtoMinor)
}

// statusRecorder captures the response status. It forwards Flush and Hijack
// and supports http.ResponseController through Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	// 1xx responses other than 101 are informational; the final status
	// follows.
	if r.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		if r.status == 0 {
			r.status = http.StatusOK
		}
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, rw, err := hijacker.Hijack()
	if err == nil && r.status == 0 {
		r.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		// net/http answers 200 when a handler writes nothing.
		return http.StatusOK
	}
	return r.status
}

// Transport returns an http.RoundTripper that propagates the current trace
// to outgoing requests (traceparent, tracestate) and, inside a traced
// operation, records a client span and http.client.request.duration
// (labels: method, status class; "error" when no response arrived). The span
// records the method, server.address, server.port and status, never the
// path, query or headers. base nil means http.DefaultTransport. With a nil
// Telemetry it returns base unchanged.
//
// Only use it for requests to services that may see your trace IDs: the
// traceparent header is sent to whatever host the request targets.
func (t *Telemetry) Transport(base http.RoundTripper) (http.RoundTripper, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	if t == nil {
		return base, nil
	}
	duration, err := t.metrics.Histogram(MetricHTTPClientDuration, "Duration of outgoing HTTP requests.", "s", DurationBuckets,
		"http.request.method", "http.response.status_class")
	if err != nil {
		return nil, err
	}
	return &clientTransport{base: base, tracer: t.Tracer(ScopeName + "/httpclient"), duration: duration}, nil
}

type clientTransport struct {
	base     http.RoundTripper
	tracer   *Tracer
	duration *Histogram
}

func (c *clientTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("telemetry: request with a URL is required")
	}
	method := NormalizeMethod(request.Method)
	ctx := request.Context()
	var span *Span
	if SpanContextFromContext(ctx).IsValid() {
		ctx, span = c.tracer.Start(ctx, method, SpanKindClient)
		if span.IsRecording() {
			span.SetAttributes(String("http.request.method", method), String("server.address", request.URL.Hostname()))
			if port := request.URL.Port(); port != "" {
				if n, err := strconv.Atoi(port); err == nil {
					span.SetAttributes(Int("server.port", n))
				}
			}
		}
	}
	// A RoundTripper must not modify the caller's request.
	outgoing := request.Clone(ctx)
	Inject(ctx, outgoing.Header)
	start := time.Now()
	response, err := c.base.RoundTrip(outgoing)
	class := "error"
	if err != nil {
		span.RecordError(err)
	} else {
		class = StatusClass(response.StatusCode)
		if span.IsRecording() {
			span.SetAttributes(Int("http.response.status_code", response.StatusCode))
			if response.StatusCode >= 400 {
				code := strconv.Itoa(response.StatusCode)
				span.SetAttributes(String("error.type", code))
				span.SetStatus(StatusError, code)
			}
		}
	}
	span.End()
	c.duration.Record(time.Since(start).Seconds(), method, class)
	return response, err
}
