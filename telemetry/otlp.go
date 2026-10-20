package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OTLP/HTTP defaults.
const (
	DefaultOTLPMaxAttempts    = 5
	DefaultOTLPInitialBackoff = 500 * time.Millisecond
	DefaultOTLPMaxBackoff     = 5 * time.Second
	maxOTLPResponseBytes      = 64 << 10
	maxRetryAfter             = 30 * time.Second
)

// OTLPOptions configure NewOTLPExporter.
type OTLPOptions struct {
	// Endpoint is the collector's base URL, such as
	// "http://otel-collector:4318". Spans are posted to Endpoint+"/v1/traces",
	// metrics to Endpoint+"/v1/metrics" and log records to
	// Endpoint+"/v1/logs". It must be http or https and carry no
	// credentials, query or fragment.
	Endpoint string
	// Headers are sent with every request, typically an API key. They are
	// never logged or included in errors.
	Headers map[string]string
	// Gzip compresses request bodies.
	Gzip bool
	// MaxAttempts bounds attempts per export, including the first. Retries
	// happen on network errors and HTTP 429, 502, 503 and 504, with
	// exponential backoff and jitter, honoring Retry-After, and never past
	// the export context's deadline. Default 5.
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// Client overrides the HTTP client, for tests. The default client
	// follows no redirects.
	Client *http.Client
}

// OTLPExporter sends spans, metrics and log records with the OTLP/HTTP
// protocol using its JSON encoding (the protobuf JSON mapping: hex trace and
// span IDs, int64 values as strings, enums as numbers). It implements
// SpanExporter, MetricExporter and LogRecordExporter and is safe for
// concurrent use.
type OTLPExporter struct {
	tracesURL      string
	metricsURL     string
	logsURL        string
	headers        http.Header
	gzip           bool
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	client         *http.Client
	closeOnce      sync.Once
}

// NewOTLPExporter validates opts and returns an exporter. It does not
// connect until the first export.
func NewOTLPExporter(opts OTLPOptions) (*OTLPExporter, error) {
	base, err := ValidateOTLPEndpoint(opts.Endpoint)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header, len(opts.Headers))
	for name, value := range opts.Headers {
		if err := validateOTLPHeader(name, value); err != nil {
			return nil, err
		}
		headers.Set(name, value)
	}
	if opts.MaxAttempts < 0 || opts.InitialBackoff < 0 || opts.MaxBackoff < 0 {
		return nil, errors.New("telemetry: OTLP retry settings must not be negative")
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = DefaultOTLPMaxAttempts
	}
	if opts.InitialBackoff == 0 {
		opts.InitialBackoff = DefaultOTLPInitialBackoff
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = DefaultOTLPMaxBackoff
	}
	client := opts.Client
	if client == nil {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("telemetry: default HTTP transport is not *http.Transport")
		}
		client = &http.Client{
			Transport: transport.Clone(),
			// A redirect could send telemetry and the authentication
			// headers to a host the operator did not configure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &OTLPExporter{
		tracesURL:      base + "/v1/traces",
		metricsURL:     base + "/v1/metrics",
		logsURL:        base + "/v1/logs",
		headers:        headers,
		gzip:           opts.Gzip,
		maxAttempts:    opts.MaxAttempts,
		initialBackoff: opts.InitialBackoff,
		maxBackoff:     max(opts.MaxBackoff, opts.InitialBackoff),
		client:         client,
	}, nil
}

// ValidateOTLPEndpoint checks a collector base URL and returns it without a
// trailing slash. Errors never quote the URL.
func ValidateOTLPEndpoint(raw string) (string, error) {
	invalid := errors.New("telemetry: OTLP endpoint must be an http:// or https:// URL without credentials, query or fragment")
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", invalid
	}
	if port := parsed.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", invalid
		}
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

var reservedOTLPHeaders = []string{"Content-Type", "Content-Encoding", "Content-Length", "Host", "Transfer-Encoding", "Connection"}

func validateOTLPHeader(name, value string) error {
	if name == "" {
		return errors.New("header name is empty")
	}
	for i := range len(name) {
		if !isTokenChar(name[i]) {
			return errors.New("header name has invalid characters")
		}
	}
	if slices.Contains(reservedOTLPHeaders, http.CanonicalHeaderKey(name)) {
		return errors.New("header name is reserved")
	}
	for i := range len(value) {
		if c := value[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return errors.New("header value has control characters")
		}
	}
	return nil
}

func isTokenChar(c byte) bool {
	return c > 0x20 && c < 0x7f && !strings.ContainsRune(`"(),/:;<=>?@[\]{}`, rune(c))
}

// ExportSpans posts spans to /v1/traces.
func (e *OTLPExporter) ExportSpans(ctx context.Context, resource Resource, spans []SpanData) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(encodeTraces(resource, spans))
	if err != nil {
		return fmt.Errorf("telemetry: encode OTLP traces: %w", err)
	}
	return e.post(ctx, e.tracesURL, body)
}

// ExportMetrics posts metrics to /v1/metrics.
func (e *OTLPExporter) ExportMetrics(ctx context.Context, resource Resource, metrics []MetricData) error {
	if len(metrics) == 0 {
		return nil
	}
	body, err := json.Marshal(encodeMetrics(resource, metrics))
	if err != nil {
		return fmt.Errorf("telemetry: encode OTLP metrics: %w", err)
	}
	return e.post(ctx, e.metricsURL, body)
}

// ExportLogs posts log records to /v1/logs.
func (e *OTLPExporter) ExportLogs(ctx context.Context, resource Resource, records []LogRecord) error {
	if len(records) == 0 {
		return nil
	}
	body, err := json.Marshal(encodeLogs(resource, records))
	if err != nil {
		return fmt.Errorf("telemetry: encode OTLP logs: %w", err)
	}
	return e.post(ctx, e.logsURL, body)
}

// Shutdown closes idle connections. Exports after Shutdown still work but
// open new connections.
func (e *OTLPExporter) Shutdown(context.Context) error {
	e.closeOnce.Do(e.client.CloseIdleConnections)
	return nil
}

// otlpError is a failed export. It carries the HTTP status only; response
// bodies may echo request data and are never included.
type otlpError struct {
	target string
	status int
	cause  error
}

func (e *otlpError) Error() string {
	if e.cause != nil {
		return "telemetry: OTLP export to " + e.target + " failed: " + e.cause.Error()
	}
	return "telemetry: OTLP export to " + e.target + " failed with HTTP " + strconv.Itoa(e.status)
}

func (e *otlpError) Unwrap() error { return e.cause }

func (e *OTLPExporter) post(ctx context.Context, target string, body []byte) error {
	contentEncoding := ""
	if e.gzip {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(body); err != nil {
			return fmt.Errorf("telemetry: compress OTLP body: %w", err)
		}
		if err := writer.Close(); err != nil {
			return fmt.Errorf("telemetry: compress OTLP body: %w", err)
		}
		body, contentEncoding = compressed.Bytes(), "gzip"
	}
	path := target[strings.LastIndex(target, "/v1/"):]
	backoff := e.initialBackoff
	for attempt := 1; ; attempt++ {
		status, retryAfter, err := e.send(ctx, target, body, contentEncoding)
		if err == nil && status >= 200 && status < 300 {
			return nil
		}
		failure := &otlpError{target: path, status: status, cause: err}
		if err == nil && !retryableStatus(status) {
			return failure
		}
		if attempt >= e.maxAttempts || ctx.Err() != nil {
			return failure
		}
		wait := jitter(backoff)
		if retryAfter > 0 {
			wait = min(retryAfter, maxRetryAfter)
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			return failure
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return failure
		case <-timer.C:
		}
		backoff = min(2*backoff, e.maxBackoff)
	}
}

func (e *OTLPExporter) send(ctx context.Context, target string, body []byte, contentEncoding string) (int, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	for name, values := range e.headers {
		request.Header[name] = values
	}
	request.Header.Set("Content-Type", "application/json")
	if contentEncoding != "" {
		request.Header.Set("Content-Encoding", contentEncoding)
	}
	response, err := e.client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// url.Error repeats the method and URL; keep the cause only.
			err = urlErr.Err
		}
		return 0, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxOTLPResponseBytes))
	return response.StatusCode, parseRetryAfter(response.Header.Get("Retry-After")), nil
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return min(wait, maxRetryAfter)
		}
	}
	return 0
}

// jitter returns a random duration in [d/2, d].
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	var buf [8]byte
	_, _ = cryptorand.Read(buf[:])
	half := uint64(d / 2)
	return d/2 + time.Duration(binary.BigEndian.Uint64(buf[:])%(half+1)) // #nosec G115 -- bounded by d/2
}

// OTLP JSON encoding (opentelemetry-proto, JSON mapping).

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string     `json:"stringValue,omitempty"`
	IntValue    *string     `json:"intValue,omitempty"`
	DoubleValue *jsonDouble `json:"doubleValue,omitempty"`
	BoolValue   *bool       `json:"boolValue,omitempty"`
}

// jsonDouble encodes non-finite values as the strings proto3 JSON uses.
type jsonDouble float64

func (d jsonDouble) MarshalJSON() ([]byte, error) {
	v := float64(d)
	switch {
	case math.IsNaN(v):
		return []byte(`"NaN"`), nil
	case math.IsInf(v, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(v, -1):
		return []byte(`"-Infinity"`), nil
	}
	return strconv.AppendFloat(nil, v, 'g', -1, 64), nil
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpTraces struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpSpan struct {
	TraceID                string         `json:"traceId"`
	SpanID                 string         `json:"spanId"`
	TraceState             string         `json:"traceState,omitempty"`
	ParentSpanID           string         `json:"parentSpanId,omitempty"`
	Flags                  uint32         `json:"flags"`
	Name                   string         `json:"name"`
	Kind                   int            `json:"kind"`
	StartTimeUnixNano      string         `json:"startTimeUnixNano"`
	EndTimeUnixNano        string         `json:"endTimeUnixNano"`
	Attributes             []otlpKeyValue `json:"attributes,omitempty"`
	DroppedAttributesCount int            `json:"droppedAttributesCount,omitempty"`
	Events                 []otlpEvent    `json:"events,omitempty"`
	DroppedEventsCount     int            `json:"droppedEventsCount,omitempty"`
	Status                 otlpStatus     `json:"status"`
}

type otlpEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpStatus struct {
	Message string `json:"message,omitempty"`
	Code    int    `json:"code,omitempty"`
}

// Span flags (opentelemetry-proto SpanFlags): the low byte holds the W3C
// trace flags; bit 8 says whether bit 9 (parent is remote) is known.
const (
	spanFlagsHasIsRemote = 0x100
	spanFlagsIsRemote    = 0x200
)

func encodeTraces(resource Resource, spans []SpanData) otlpTraces {
	byScope := make(map[string][]otlpSpan)
	for _, span := range spans {
		encoded := otlpSpan{
			TraceID:                span.SpanContext.TraceID.String(),
			SpanID:                 span.SpanContext.SpanID.String(),
			TraceState:             span.SpanContext.TraceState,
			Flags:                  uint32(span.SpanContext.Flags) | spanFlagsHasIsRemote,
			Name:                   span.Name,
			Kind:                   int(span.Kind),
			StartTimeUnixNano:      unixNano(span.Start),
			EndTimeUnixNano:        unixNano(span.End),
			Attributes:             encodeAttributes(span.Attributes),
			DroppedAttributesCount: span.DroppedAttributes,
			DroppedEventsCount:     span.DroppedEvents,
			Status:                 otlpStatus{Code: int(span.Status), Message: span.StatusDescription},
		}
		if span.ParentSpanID.IsValid() {
			encoded.ParentSpanID = span.ParentSpanID.String()
			if span.ParentRemote {
				encoded.Flags |= spanFlagsIsRemote
			}
		}
		for _, event := range span.Events {
			encoded.Events = append(encoded.Events, otlpEvent{TimeUnixNano: unixNano(event.Time), Name: event.Name, Attributes: encodeAttributes(event.Attributes)})
		}
		byScope[span.Scope] = append(byScope[span.Scope], encoded)
	}
	scopes := make([]string, 0, len(byScope))
	for scope := range byScope {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	group := otlpResourceSpans{Resource: otlpResource{Attributes: encodeAttributes(resource.Attributes)}}
	for _, scope := range scopes {
		group.ScopeSpans = append(group.ScopeSpans, otlpScopeSpans{Scope: otlpScope{Name: scope}, Spans: byScope[scope]})
	}
	return otlpTraces{ResourceSpans: []otlpResourceSpans{group}}
}

type otlpMetrics struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Gauge       *otlpGauge     `json:"gauge,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

// aggregationCumulative is AGGREGATION_TEMPORALITY_CUMULATIVE.
const aggregationCumulative = 2

type otlpSum struct {
	DataPoints             []otlpNumberPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type otlpGauge struct {
	DataPoints []otlpNumberPoint `json:"dataPoints"`
}

type otlpHistogram struct {
	DataPoints             []otlpHistogramPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality"`
}

type otlpNumberPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	AsInt             *string        `json:"asInt,omitempty"`
	AsDouble          *jsonDouble    `json:"asDouble,omitempty"`
}

type otlpHistogramPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             string         `json:"count"`
	Sum               jsonDouble     `json:"sum"`
	BucketCounts      []string       `json:"bucketCounts"`
	ExplicitBounds    []float64      `json:"explicitBounds"`
}

func encodeMetrics(resource Resource, metrics []MetricData) otlpMetrics {
	scope := otlpScopeMetrics{Scope: otlpScope{Name: ScopeName}}
	for _, metric := range metrics {
		encoded := otlpMetric{Name: metric.Name, Description: metric.Description, Unit: metric.Unit}
		switch metric.Kind {
		case MetricCounter:
			sum := &otlpSum{AggregationTemporality: aggregationCumulative, IsMonotonic: true}
			for _, p := range metric.Points {
				point := otlpNumberPoint{Attributes: encodeAttributes(p.Labels), StartTimeUnixNano: unixNano(p.Start), TimeUnixNano: unixNano(p.Time)}
				if metric.Fractional {
					value := jsonDouble(p.Value)
					point.AsDouble = &value
				} else {
					value := strconv.FormatUint(uint64(p.Value), 10)
					point.AsInt = &value
				}
				sum.DataPoints = append(sum.DataPoints, point)
			}
			encoded.Sum = sum
		case MetricGauge:
			gauge := &otlpGauge{}
			for _, p := range metric.Points {
				value := jsonDouble(p.Value)
				gauge.DataPoints = append(gauge.DataPoints, otlpNumberPoint{Attributes: encodeAttributes(p.Labels), TimeUnixNano: unixNano(p.Time), AsDouble: &value})
			}
			encoded.Gauge = gauge
		case MetricHistogram:
			histogram := &otlpHistogram{AggregationTemporality: aggregationCumulative}
			for _, p := range metric.Points {
				counts := make([]string, len(p.BucketCounts))
				for i, count := range p.BucketCounts {
					counts[i] = strconv.FormatUint(count, 10)
				}
				histogram.DataPoints = append(histogram.DataPoints, otlpHistogramPoint{
					Attributes:        encodeAttributes(p.Labels),
					StartTimeUnixNano: unixNano(p.Start),
					TimeUnixNano:      unixNano(p.Time),
					Count:             strconv.FormatUint(p.Count, 10),
					Sum:               jsonDouble(p.Sum),
					BucketCounts:      counts,
					ExplicitBounds:    metric.Bounds,
				})
			}
			encoded.Histogram = histogram
		}
		scope.Metrics = append(scope.Metrics, encoded)
	}
	return otlpMetrics{ResourceMetrics: []otlpResourceMetrics{{
		Resource:     otlpResource{Attributes: encodeAttributes(resource.Attributes)},
		ScopeMetrics: []otlpScopeMetrics{scope},
	}}}
}

type otlpLogs struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpLogRecord struct {
	TimeUnixNano           string         `json:"timeUnixNano"`
	ObservedTimeUnixNano   string         `json:"observedTimeUnixNano"`
	SeverityNumber         int            `json:"severityNumber"`
	SeverityText           string         `json:"severityText,omitempty"`
	Body                   otlpAnyValue   `json:"body"`
	Attributes             []otlpKeyValue `json:"attributes,omitempty"`
	DroppedAttributesCount int            `json:"droppedAttributesCount,omitempty"`
	Flags                  uint32         `json:"flags,omitempty"`
	TraceID                string         `json:"traceId,omitempty"`
	SpanID                 string         `json:"spanId,omitempty"`
}

func encodeLogs(resource Resource, records []LogRecord) otlpLogs {
	scope := otlpScopeLogs{Scope: otlpScope{Name: ScopeName}, LogRecords: make([]otlpLogRecord, 0, len(records))}
	for _, record := range records {
		body := record.Body
		encoded := otlpLogRecord{
			TimeUnixNano:           unixNano(record.Time),
			ObservedTimeUnixNano:   unixNano(record.ObservedTime),
			SeverityNumber:         record.Severity,
			SeverityText:           record.SeverityText,
			Body:                   otlpAnyValue{StringValue: &body},
			Attributes:             encodeAttributes(record.Attributes),
			DroppedAttributesCount: record.DroppedAttributes,
		}
		if record.TraceID.IsValid() {
			encoded.TraceID = record.TraceID.String()
			encoded.Flags = uint32(record.Flags)
			if record.SpanID.IsValid() {
				encoded.SpanID = record.SpanID.String()
			}
		}
		scope.LogRecords = append(scope.LogRecords, encoded)
	}
	return otlpLogs{ResourceLogs: []otlpResourceLogs{{
		Resource:  otlpResource{Attributes: encodeAttributes(resource.Attributes)},
		ScopeLogs: []otlpScopeLogs{scope},
	}}}
}

func encodeAttributes(attrs []Attribute) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	encoded := make([]otlpKeyValue, 0, len(attrs))
	for _, attr := range attrs {
		var value otlpAnyValue
		switch attr.Value.Kind() {
		case KindString:
			s := attr.Value.AsString()
			value.StringValue = &s
		case KindInt64:
			s := strconv.FormatInt(attr.Value.AsInt64(), 10)
			value.IntValue = &s
		case KindFloat64:
			d := jsonDouble(attr.Value.AsFloat64())
			value.DoubleValue = &d
		case KindBool:
			b := attr.Value.AsBool()
			value.BoolValue = &b
		default:
			continue
		}
		encoded = append(encoded, otlpKeyValue{Key: attr.Key, Value: value})
	}
	return encoded
}

func unixNano(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}
