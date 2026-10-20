// Package telemetry is Forge's dependency-free observability core: W3C Trace
// Context propagation, a minimal tracer with parent-based ratio sampling, a
// bounded span batcher, metric instruments with a cardinality cap, a
// Prometheus text handler, an OTLP/HTTP JSON exporter and a log handler that
// correlates log records with traces (ADR 0034, docs/OBSERVABILITY.md).
//
// It implements the subset of OpenTelemetry that a Forge application needs,
// on the standard library only, and speaks the OpenTelemetry wire protocol so
// any OpenTelemetry Collector or compatible backend can receive the data.
//
// A nil *Telemetry is valid and disabled: every method is a no-op, tracers
// start no spans and instruments record nothing. Instrumented Forge
// components take a *Telemetry and pay nothing when it is nil.
//
// Nothing here records request or response bodies, query strings, raw URL
// paths, headers, cookies, SQL text, SQL arguments, job payloads, mail
// recipients or error messages.
package telemetry

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for Options fields left zero.
const (
	DefaultMaxQueueSize   = 2048
	DefaultMaxExportBatch = 512
	DefaultBatchTimeout   = 5 * time.Second
	// DefaultLogBatchTimeout is shorter than the span interval: people tail
	// logs.
	DefaultLogBatchTimeout = time.Second
	DefaultExportTimeout   = 10 * time.Second
	DefaultMetricsInterval = 60 * time.Second
	minMetricsInterval     = time.Second
	maxQueueSizeLimit      = 1 << 20
)

// ScopeName is the instrumentation scope of Forge's own instrumentation.
const ScopeName = "github.com/rocketinsidecorp/forge-go"

// Resource describes the process that produces telemetry.
type Resource struct {
	Attributes []Attribute
}

// MetricExporter sends metric snapshots to a backend. It must honor ctx.
// Shutdown is called once, after the last export (see SpanExporter).
type MetricExporter interface {
	ExportMetrics(ctx context.Context, resource Resource, metrics []MetricData) error
	Shutdown(ctx context.Context) error
}

// Options configure New. Zero values select the documented defaults.
type Options struct {
	// ServiceName is the service.name resource attribute. Required.
	ServiceName    string
	ServiceVersion string
	// Environment is the deployment.environment.name resource attribute.
	Environment string
	// SampleRatio is the fraction, 0 to 1, of new traces that are sampled.
	// Spans whose parent exists follow the parent's decision.
	SampleRatio float64
	// TrustIncoming makes the HTTP middleware continue traces from the
	// traceparent and tracestate headers of incoming requests. When false
	// (the default) every request starts a new trace, so clients cannot
	// choose trace IDs, force sampling or forward tracestate downstream.
	// Enable it only behind a proxy that sets or sanitizes these headers.
	TrustIncoming bool
	// SpanExporter receives sampled spans. Nil means spans are never
	// recorded: they still carry identifiers for log correlation and
	// propagation, at the cost of an ID and a small allocation per span.
	SpanExporter SpanExporter
	// MetricExporter receives a snapshot every MetricsInterval and at
	// shutdown. Nil means metrics are only available through
	// Registry.PrometheusHandler.
	MetricExporter  MetricExporter
	MetricsInterval time.Duration
	// MaxQueueSize bounds the spans waiting for export; when the queue is
	// full new spans are dropped and counted. Default 2048.
	MaxQueueSize int
	// MaxExportBatch is the most spans sent in one export. Default 512.
	MaxExportBatch int
	// BatchTimeout is the longest a span waits before export. Default 5s.
	BatchTimeout time.Duration
	// ExportTimeout bounds one export call. Default 10s.
	ExportTimeout time.Duration
	// LogExporter receives log records queued by LogHandler. Nil means
	// logs are not exported and LogHandler returns nil.
	LogExporter LogRecordExporter
	// LogLevel is the lowest level LogHandler queues. Default slog.LevelInfo.
	LogLevel slog.Leveler
	// LogFormat is how LogHandler encodes values that are not strings,
	// numbers, booleans, durations or times. Set it to the format of the
	// handler that writes standard output: LogFormatJSON (default) for
	// slog.JSONHandler, LogFormatText for slog.TextHandler. FromConfig sets
	// it from the logger it is given (see LogOutputHandler).
	LogFormat LogFormat
	// LogReplaceAttr is the ReplaceAttr of the slog.HandlerOptions of the
	// handler that writes standard output, or nil. LogHandler applies it
	// exactly as that handler does, so an attribute redacted or removed on
	// standard output is redacted or removed in the export too. Set it
	// whenever the standard-output handler has a ReplaceAttr: slog handlers
	// do not reveal their options. FromConfig sets it from a logger whose
	// handler is a LogOutputHandler, such as NewOutputHandler's.
	LogReplaceAttr func(groups []string, a slog.Attr) slog.Attr
	// LogMaxQueueSize bounds the log records waiting for export; when the
	// queue is full new records are dropped and counted. Default 2048.
	LogMaxQueueSize int
	// LogMaxExportBatch is the most records sent in one export. Default 512.
	LogMaxExportBatch int
	// LogBatchTimeout is the longest a record waits before export. Default
	// 1s.
	LogBatchTimeout time.Duration
	// MaxSeries bounds the series of each metric instrument. Default 1000.
	MaxSeries int
	// Logger receives export failures. Default discards. It must not export
	// to LogExporter: a failed log export would log, and so export, again.
	// forge.New passes the logger that writes to standard output only.
	Logger *slog.Logger
}

// Telemetry owns the tracer state, the metrics registry and the exporters'
// goroutines. Create it with New, share it, and call Shutdown once when the
// process stops (forge.App does both).
type Telemetry struct {
	resource      Resource
	threshold     uint64
	trustIncoming bool
	logger        *slog.Logger
	spans         *batcher[SpanData]
	logs          *batcher[LogRecord]
	logLevel      slog.Leveler
	logFormat     LogFormat
	logReplace    func(groups []string, a slog.Attr) slog.Attr
	metrics       *Registry

	metricExporter MetricExporter
	metricsRun     context.Context
	metricsCancel  context.CancelFunc
	metricsStop    chan struct{}
	metricsDone    chan struct{}
	// shutdownMetricExporter releases the metric exporter once the metrics
	// goroutine stopped; metricsShutdownErr is its result, read after
	// metricsDone is closed.
	shutdownMetricExporter func(context.Context) error
	metricsShutdownErr     error
	exportTimeout          time.Duration

	tracersMu sync.Mutex
	tracers   map[string]*Tracer

	// pools holds each observed pool name's wait totals (ObservePool).
	poolsMu sync.Mutex
	pools   map[string]*poolTotals

	shutdownOnce sync.Once
	shutdownErr  error

	droppedSpans *Counter
	droppedLogs  *Counter
	exportErrors *Counter
}

// New validates opts and starts the exporters' goroutines, if any.
func New(opts Options) (*Telemetry, error) {
	if opts.ServiceName == "" {
		return nil, errors.New("telemetry: service name is required")
	}
	if opts.SampleRatio < 0 || opts.SampleRatio > 1 || opts.SampleRatio != opts.SampleRatio {
		return nil, errors.New("telemetry: sample ratio must be between 0 and 1")
	}
	if opts.MaxQueueSize < 0 || opts.MaxQueueSize > maxQueueSizeLimit || opts.MaxExportBatch < 0 ||
		opts.BatchTimeout < 0 || opts.ExportTimeout < 0 || opts.MetricsInterval < 0 ||
		opts.LogMaxQueueSize < 0 || opts.LogMaxQueueSize > maxQueueSizeLimit || opts.LogMaxExportBatch < 0 || opts.LogBatchTimeout < 0 {
		return nil, errors.New("telemetry: queue sizes and intervals must not be negative or too large")
	}
	if opts.LogFormat > LogFormatText {
		return nil, errors.New("telemetry: unknown log format")
	}
	if opts.MaxQueueSize == 0 {
		opts.MaxQueueSize = DefaultMaxQueueSize
	}
	if opts.MaxExportBatch == 0 {
		opts.MaxExportBatch = DefaultMaxExportBatch
	}
	opts.MaxExportBatch = min(opts.MaxExportBatch, opts.MaxQueueSize)
	if opts.BatchTimeout == 0 {
		opts.BatchTimeout = DefaultBatchTimeout
	}
	if opts.LogMaxQueueSize == 0 {
		opts.LogMaxQueueSize = DefaultMaxQueueSize
	}
	if opts.LogMaxExportBatch == 0 {
		opts.LogMaxExportBatch = DefaultMaxExportBatch
	}
	opts.LogMaxExportBatch = min(opts.LogMaxExportBatch, opts.LogMaxQueueSize)
	if opts.LogBatchTimeout == 0 {
		opts.LogBatchTimeout = DefaultLogBatchTimeout
	}
	if opts.LogLevel == nil {
		opts.LogLevel = slog.LevelInfo
	}
	if opts.ExportTimeout == 0 {
		opts.ExportTimeout = DefaultExportTimeout
	}
	if opts.MetricsInterval == 0 {
		opts.MetricsInterval = DefaultMetricsInterval
	}
	if opts.MetricsInterval < minMetricsInterval {
		return nil, fmt.Errorf("telemetry: metrics interval must be at least %s", minMetricsInterval)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	attrs := []Attribute{
		String("service.name", opts.ServiceName),
		String("telemetry.sdk.name", "forge"),
		String("telemetry.sdk.language", "go"),
	}
	if opts.ServiceVersion != "" {
		attrs = append(attrs, String("service.version", opts.ServiceVersion))
	}
	if opts.Environment != "" {
		attrs = append(attrs, String("deployment.environment.name", opts.Environment))
	}
	t := &Telemetry{
		resource:       Resource{Attributes: attrs},
		threshold:      ratioThreshold(opts.SampleRatio),
		trustIncoming:  opts.TrustIncoming,
		logger:         opts.Logger,
		metrics:        NewRegistry(opts.MaxSeries),
		metricExporter: opts.MetricExporter,
		exportTimeout:  opts.ExportTimeout,
		tracers:        make(map[string]*Tracer),
		logLevel:       opts.LogLevel,
		logFormat:      opts.LogFormat,
		logReplace:     opts.LogReplaceAttr,
	}
	var err error
	if t.droppedSpans, err = t.metrics.Counter("forge.telemetry.spans.dropped", "Spans dropped because the export queue was full or export failed.", "{span}"); err != nil {
		return nil, err
	}
	if t.droppedLogs, err = t.metrics.Counter("forge.telemetry.logs.dropped", "Log records dropped because the export queue was full or export failed.", "{log_record}"); err != nil {
		return nil, err
	}
	if t.exportErrors, err = t.metrics.Counter("forge.telemetry.export.errors", "Failed export calls.", "{error}", "signal"); err != nil {
		return nil, err
	}
	if err := registerRuntimeMetrics(t.metrics); err != nil {
		return nil, err
	}
	// Each exporter is shut down once, by the last signal using it to stop.
	var shutdowns exporterShutdowns
	var spanShutdown, logShutdown func(context.Context) error
	if opts.SpanExporter != nil {
		spanShutdown = shutdowns.add(opts.SpanExporter, opts.SpanExporter.Shutdown)
	}
	if opts.LogExporter != nil {
		logShutdown = shutdowns.add(opts.LogExporter, opts.LogExporter.Shutdown)
	}
	if opts.MetricExporter != nil {
		t.shutdownMetricExporter = shutdowns.add(opts.MetricExporter, opts.MetricExporter.Shutdown)
	}
	if opts.SpanExporter != nil {
		onError := func(n int) {
			t.exportErrors.Add(1, "traces")
			t.droppedSpans.Add(int64(n))
		}
		t.spans = newBatcher(batcherConfig[SpanData]{
			signal:           "span",
			failedMessage:    "telemetry: span export failed; spans dropped",
			countKey:         "spans",
			export:           opts.SpanExporter.ExportSpans,
			shutdownExporter: spanShutdown,
			resource:         t.resource,
			logger:           opts.Logger,
			maxQueue:         opts.MaxQueueSize,
			maxBatch:         opts.MaxExportBatch,
			interval:         opts.BatchTimeout,
			timeout:          opts.ExportTimeout,
			onError:          onError,
		})
	}
	if opts.LogExporter != nil {
		onError := func(n int) {
			t.exportErrors.Add(1, "logs")
			t.droppedLogs.Add(int64(n))
		}
		t.logs = newBatcher(batcherConfig[LogRecord]{
			signal:           "log",
			failedMessage:    "telemetry: log export failed; log records dropped",
			countKey:         "log_records",
			export:           opts.LogExporter.ExportLogs,
			shutdownExporter: logShutdown,
			resource:         t.resource,
			logger:           opts.Logger,
			maxQueue:         opts.LogMaxQueueSize,
			maxBatch:         opts.LogMaxExportBatch,
			interval:         opts.LogBatchTimeout,
			timeout:          opts.ExportTimeout,
			onError:          onError,
		})
	}
	if opts.MetricExporter != nil {
		t.metricsRun, t.metricsCancel = context.WithCancel(context.Background())
		t.metricsStop = make(chan struct{})
		t.metricsDone = make(chan struct{})
		go t.exportMetricsLoop(opts.MetricsInterval)
	}
	return t, nil
}

// Tracer returns the tracer for an instrumentation scope, such as a package
// import path. A nil Telemetry returns a nil Tracer, which starts no spans.
func (t *Telemetry) Tracer(scope string) *Tracer {
	if t == nil {
		return nil
	}
	t.tracersMu.Lock()
	defer t.tracersMu.Unlock()
	tracer, ok := t.tracers[scope]
	if !ok {
		tracer = &Tracer{telemetry: t, scope: scope}
		t.tracers[scope] = tracer
	}
	return tracer
}

// Metrics returns the metrics registry, or nil for a nil Telemetry.
func (t *Telemetry) Metrics() *Registry {
	if t == nil {
		return nil
	}
	return t.metrics
}

// Resource returns the resource attributes sent with every export.
func (t *Telemetry) Resource() Resource {
	if t == nil {
		return Resource{}
	}
	return t.resource
}

// TrustIncoming reports whether incoming traceparent headers are honored.
func (t *Telemetry) TrustIncoming() bool { return t != nil && t.trustIncoming }

// Stats reports span export counters.
type Stats struct {
	// Dropped spans: queue full, export failed, or shutdown deadline passed.
	Dropped  uint64
	Exported uint64
	// Failed is the number of spans in failed export calls (included in
	// Dropped).
	Failed uint64
}

// Stats returns span export counters; all zero without a span exporter.
func (t *Telemetry) Stats() Stats {
	if t == nil || t.spans == nil {
		return Stats{}
	}
	failed := t.spans.failed.Load()
	return Stats{Dropped: t.spans.dropped.Load() + failed, Exported: t.spans.exported.Load(), Failed: failed}
}

// ForceFlush exports queued spans and log records and a metric snapshot now, waiting at most
// until ctx ends.
func (t *Telemetry) ForceFlush(ctx context.Context) error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.spans != nil {
		errs = append(errs, t.spans.flush(ctx))
	}
	if t.logs != nil {
		errs = append(errs, t.logs.flush(ctx))
	}
	if t.metricExporter != nil {
		errs = append(errs, t.exportMetrics(ctx))
	}
	return errors.Join(errs...)
}

// Shutdown exports what is queued, stops the exporter goroutines and shuts
// the exporters down, all within ctx. The signals shut down concurrently, so
// a hung collector costs each at most the whole budget but not their sum; an
// exporter shared by several signals is shut down once, after the last of
// them stopped.
// Spans and log records still queued when ctx ends are dropped, and so are
// those produced after Shutdown. Only the first call has an
// effect; later calls return its result. Its signature matches
// forge.ShutdownHook.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.shutdownOnce.Do(func() {
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			errs []error
		)
		run := func(stop func(context.Context) error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := stop(ctx); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}()
		}
		if t.spans != nil {
			run(t.spans.shutdown)
		}
		if t.logs != nil {
			run(t.logs.shutdown)
		}
		if t.metricExporter != nil {
			run(t.stopMetrics)
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.shutdownErr = fmt.Errorf("telemetry shutdown: %w", err)
		}
	})
	return t.shutdownErr
}

// stopMetrics asks the metrics goroutine for a final export and waits for it
// at most until ctx ends, cancelling the export in progress at that point.
func (t *Telemetry) stopMetrics(ctx context.Context) error {
	stopCancel := context.AfterFunc(ctx, t.metricsCancel)
	defer stopCancel()
	close(t.metricsStop)
	select {
	case <-t.metricsDone:
		t.metricsCancel()
		return t.metricsShutdownErr
	case <-ctx.Done():
		t.metricsCancel()
		return fmt.Errorf("telemetry: metric export did not finish before the shutdown deadline: %w", ctx.Err())
	}
}

func (t *Telemetry) exportMetricsLoop(interval time.Duration) {
	defer close(t.metricsDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := t.exportMetrics(t.metricsRun); err != nil {
				t.logger.Warn("telemetry: metric export failed", "error_type", ErrorType(err), "error", err)
			}
		case <-t.metricsStop:
			// The final snapshot carries the counts of the last interval.
			if err := t.exportMetrics(t.metricsRun); err != nil {
				t.logger.Warn("telemetry: final metric export failed", "error_type", ErrorType(err), "error", err)
			}
			// After the last export, like the batchers.
			t.metricsShutdownErr = t.shutdownMetricExporter(t.metricsRun)
			return
		}
	}
}

func (t *Telemetry) exportMetrics(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, t.exportTimeout)
	defer cancel()
	err := t.metricExporter.ExportMetrics(ctx, t.resource, t.metrics.Collect())
	if err != nil {
		t.exportErrors.Add(1, "metrics")
	}
	return err
}

func (t *Telemetry) newSpan(tracer *Tracer, parent SpanContext, start time.Time, name string, kind SpanKind, attrs []Attribute) *Span {
	span := &Span{tracer: tracer}
	if parent.IsValid() {
		span.sc = SpanContext{TraceID: parent.TraceID, SpanID: newSpanID(), Flags: parent.Flags, TraceState: parent.TraceState}
		span.parent = parent.SpanID
		span.parentRemote = parent.Remote
	} else {
		traceID, spanID := newIDs()
		span.sc = SpanContext{TraceID: traceID, SpanID: spanID}
		if t.sampleRoot(traceID) {
			span.sc.Flags = FlagSampled
		}
	}
	if !span.sc.IsSampled() || t.spans == nil {
		return span
	}
	span.recording = true
	span.name = name
	span.kind = kind
	span.start = start
	span.attrs = make([]Attribute, 0, len(attrs)+8)
	span.addAttributes(attrs)
	return span
}

func (t *Telemetry) exportSpan(data SpanData) {
	if t.spans == nil {
		return
	}
	if !t.spans.enqueue(data) {
		t.droppedSpans.Add(1)
	}
}

// ratioThreshold implements the OpenTelemetry TraceIDRatioBased sampler: a
// root span is sampled when the trace ID's last 8 bytes, shifted right by
// one, are below ratio * 2^63.
func ratioThreshold(ratio float64) uint64 {
	if ratio >= 1 {
		return 1 << 63
	}
	return uint64(ratio * (1 << 63))
}

func (t *Telemetry) sampleRoot(id TraceID) bool {
	if t.threshold >= 1<<63 {
		return true
	}
	return binary.BigEndian.Uint64(id[8:])>>1 < t.threshold
}

// exporterShutdowns shuts each exporter down once, after every signal that
// exports through it has stopped: FromConfig gives one OTLPExporter to
// traces, metrics and logs, which stop concurrently, and Shutdown must not
// run while another signal still exports.
type exporterShutdowns struct {
	shared []*sharedExporter
}

type sharedExporter struct {
	exporter any
	shutdown func(context.Context) error
	// users is the number of signals that have not stopped yet.
	users atomic.Int32
}

// add registers one signal exporting through exporter and returns the
// function that signal calls once, after its last export; the last such call
// shuts the exporter down. All adds happen before any signal can stop.
func (s *exporterShutdowns) add(exporter any, shutdown func(context.Context) error) func(context.Context) error {
	var shared *sharedExporter
	for _, candidate := range s.shared {
		if sameExporter(candidate.exporter, exporter) {
			shared = candidate
			break
		}
	}
	if shared == nil {
		shared = &sharedExporter{exporter: exporter, shutdown: shutdown}
		s.shared = append(s.shared, shared)
	}
	shared.users.Add(1)
	return func(ctx context.Context) error {
		if shared.users.Add(-1) == 0 {
			return shared.shutdown(ctx)
		}
		return nil
	}
}

// sameExporter reports whether a and b are the same exporter. Values that
// cannot be compared (a struct holding a slice, say) are never the same,
// rather than panicking.
func sameExporter(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	return va.Type() == vb.Type() && va.Comparable() && vb.Comparable() && a == b
}
