package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

// recordingLogExporter keeps exported log records. block, when set, makes
// exports wait until it is closed or the export context ends.
type recordingLogExporter struct {
	mu       sync.Mutex
	records  []LogRecord
	batches  []int
	block    chan struct{}
	err      error
	shutdown int
}

func (e *recordingLogExporter) ExportLogs(ctx context.Context, _ Resource, records []LogRecord) error {
	if e.block != nil {
		select {
		case <-e.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.records = append(e.records, records...)
	e.batches = append(e.batches, len(records))
	return nil
}

func (e *recordingLogExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdown++
	return nil
}

func (e *recordingLogExporter) exported() []LogRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]LogRecord(nil), e.records...)
}

func logsLogger(tel *Telemetry) *slog.Logger { return slog.New(tel.LogHandler()) }

func attrMap(record LogRecord) map[string]string {
	attrs := make(map[string]string, len(record.Attributes))
	for _, attr := range record.Attributes {
		attrs[attr.Key] = attr.Value.Emit()
	}
	return attrs
}

func TestLogsAreDisabledWithoutAnExporter(t *testing.T) {
	tel := newTestTelemetry(t, Options{})
	if tel.LogHandler() != nil {
		t.Fatal("LogHandler must be nil without an exporter")
	}
	var nilTelemetry *Telemetry
	if nilTelemetry.LogHandler() != nil || nilTelemetry.LogStats() != (Stats{}) {
		t.Fatal("a nil Telemetry has no log handler")
	}
}

func TestOTLPLogExportCarriesTraceContextSeverityAndResource(t *testing.T) {
	c, server := newCollector(t)
	exporter := newExporter(t, server.URL, OTLPOptions{Headers: map[string]string{"Authorization": "Bearer s3cr3t"}})
	tel := newTestTelemetry(t, Options{ServiceName: "billing", ServiceVersion: "1.2.3", Environment: "staging", SampleRatio: 1, LogExporter: exporter, LogLevel: slog.LevelDebug})
	logger := logsLogger(tel)
	ctx, span := tel.Tracer("x").Start(context.Background(), "op", SpanKindInternal)

	logger.InfoContext(ctx, "charged", "amount", 12, "ok", true, "ratio", 0.5, "took", 1500*time.Millisecond)
	logger.With("component", "billing").WithGroup("http").Warn("slow", slog.Group("req", "status", 503))
	logger.Debug("detail")
	logger.Error("boom", "error", errors.New("card declined"))
	shutdown(t, tel)

	requests := c.received()
	if len(requests) != 1 || requests[0].path != "/v1/logs" || requests[0].header.Get("Authorization") != "Bearer s3cr3t" {
		t.Fatalf("requests %+v", requests)
	}
	payload := requests[0].payload
	resource, _ := json.Marshal(path(t, payload, "resourceLogs", 0, "resource", "attributes"))
	for _, want := range []string{`"service.name"`, `"billing"`, `"service.version"`, `"deployment.environment.name"`, `"staging"`} {
		if !strings.Contains(string(resource), want) {
			t.Fatalf("resource %s lacks %s", resource, want)
		}
	}
	records := path(t, payload, "resourceLogs", 0, "scopeLogs", 0, "logRecords").([]any)
	if len(records) != 4 {
		t.Fatalf("records: %v", records)
	}
	first := records[0].(map[string]any)
	if first["traceId"] != span.SpanContext().TraceID.String() || first["spanId"] != span.SpanContext().SpanID.String() || first["flags"] != float64(1) {
		t.Fatalf("trace context: %v", first)
	}
	if first["severityNumber"] != float64(9) || first["severityText"] != "INFO" || path(t, first, "body", "stringValue") != "charged" {
		t.Fatalf("record: %v", first)
	}
	if first["timeUnixNano"] == "0" || first["observedTimeUnixNano"] == "0" {
		t.Fatalf("times: %v", first)
	}
	encoded, _ := json.Marshal(first["attributes"])
	for _, want := range []string{`{"key":"amount","value":{"intValue":"12"}}`, `{"key":"ok","value":{"boolValue":true}}`, `{"key":"ratio","value":{"doubleValue":0.5}}`, `{"key":"took","value":{"stringValue":"1.5s"}}`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("attributes %s lack %s", encoded, want)
		}
	}
	second := records[1].(map[string]any)
	if second["severityNumber"] != float64(13) || second["traceId"] != nil {
		t.Fatalf("second: %v", second)
	}
	encoded, _ = json.Marshal(second["attributes"])
	for _, want := range []string{`"component"`, `"http.req.status"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("grouped attributes %s lack %s", encoded, want)
		}
	}
	if records[2].(map[string]any)["severityNumber"] != float64(5) || records[3].(map[string]any)["severityNumber"] != float64(17) {
		t.Fatalf("severities: %v", records)
	}
	if encoded, _ := json.Marshal(records[3]); !strings.Contains(string(encoded), "card declined") {
		t.Fatalf("an error attribute is exported as it is written to stdout: %s", encoded)
	}
}

func TestLogTraceContextFromAttributesMovesToTheRecord(t *testing.T) {
	exporter := &recordingLogExporter{}
	tel := newTestTelemetry(t, Options{LogExporter: exporter})
	traceID, spanID := TraceID{1, 15: 2}, SpanID{7: 3}
	logger := logsLogger(tel).With(LogTraceIDKey, traceID.String(), LogSpanIDKey, spanID.String())
	logger.Info("request log")
	// A trace_id that is not a valid identifier stays an attribute.
	logsLogger(tel).Info("odd", LogTraceIDKey, "not-hex")
	// With a traced context the context wins and attributes do not duplicate.
	ctx, span := newTestTelemetry(t, Options{SampleRatio: 1}).Tracer("x").Start(context.Background(), "op", SpanKindInternal)
	logger.InfoContext(ctx, "both")
	shutdown(t, tel)

	records := exporter.exported()
	if len(records) != 3 {
		t.Fatalf("records: %d", len(records))
	}
	if records[0].TraceID != traceID || records[0].SpanID != spanID || len(records[0].Attributes) != 0 {
		t.Fatalf("attribute correlation: %+v", records[0])
	}
	if records[1].TraceID.IsValid() || attrMap(records[1])[LogTraceIDKey] != "not-hex" {
		t.Fatalf("invalid trace_id must stay an attribute: %+v", records[1])
	}
	if records[2].TraceID != span.SpanContext().TraceID || len(records[2].Attributes) != 0 {
		t.Fatalf("context correlation: %+v", records[2])
	}
}

func TestSeverityMapping(t *testing.T) {
	for _, tc := range []struct {
		level  slog.Level
		number int
		text   string
	}{
		{slog.LevelDebug - 4, 1, "TRACE"}, {slog.LevelDebug - 1, 4, "TRACE"}, {slog.LevelDebug - 40, 1, "TRACE"},
		{slog.LevelDebug, 5, "DEBUG"}, {slog.LevelInfo - 1, 8, "DEBUG"},
		{slog.LevelInfo, 9, "INFO"}, {slog.LevelInfo + 2, 11, "INFO"},
		{slog.LevelWarn, 13, "WARN"}, {slog.LevelWarn + 3, 16, "WARN"},
		{slog.LevelError, 17, "ERROR"}, {slog.LevelError + 3, 20, "ERROR"},
		{slog.LevelError + 4, 21, "FATAL"}, {slog.LevelError + 40, 24, "FATAL"},
	} {
		if number, text := severityOf(tc.level); number != tc.number || text != tc.text {
			t.Errorf("level %d = %d %s, want %d %s", tc.level, number, text, tc.number, tc.text)
		}
	}
}

func TestLogLevelFiltersExport(t *testing.T) {
	exporter := &recordingLogExporter{}
	tel := newTestTelemetry(t, Options{LogExporter: exporter, LogLevel: slog.LevelWarn})
	handler := tel.LogHandler()
	if handler.Enabled(context.Background(), slog.LevelInfo) || !handler.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("level not honored")
	}
	logger := slog.New(handler)
	logger.Info("quiet")
	logger.Warn("loud")
	shutdown(t, tel)
	if got := exporter.exported(); len(got) != 1 || got[0].Body != "loud" {
		t.Fatalf("exported %+v", got)
	}
}

func TestLogRecordsAreBounded(t *testing.T) {
	exporter := &recordingLogExporter{}
	tel := newTestTelemetry(t, Options{LogExporter: exporter})
	args := make([]any, 0, 2*(MaxLogAttributes+10))
	for i := range MaxLogAttributes + 10 {
		args = append(args, fmt.Sprintf("k%d", i), "v")
	}
	logsLogger(tel).Info(strings.Repeat("m", MaxLogBodyLength*2), append(args, "long", strings.Repeat("v", 5*MaxAttributeValueLength))...)
	shutdown(t, tel)
	record := exporter.exported()[0]
	if len(record.Body) != MaxLogBodyLength || len(record.Attributes) != MaxLogAttributes || record.DroppedAttributes != 11 {
		t.Fatalf("body %d, attributes %d, dropped %d", len(record.Body), len(record.Attributes), record.DroppedAttributes)
	}
	for _, attr := range record.Attributes {
		if len(attr.Value.Emit()) > MaxAttributeValueLength {
			t.Fatalf("attribute %s not truncated", attr.Key)
		}
	}
}

func TestLogExportNeverBlocksWhenTheQueueIsFullAndCountsDrops(t *testing.T) {
	exporter := &recordingLogExporter{block: make(chan struct{})}
	tel := newTestTelemetry(t, Options{LogExporter: exporter, LogMaxQueueSize: 8, LogMaxExportBatch: 4, LogBatchTimeout: time.Hour})
	logger := logsLogger(tel)
	// The first full batch blocks in the exporter; the queue then fills.
	for range 4 {
		logger.Info("x")
	}
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	for range 100 {
		logger.Info("y")
	}
	if time.Since(start) > time.Second {
		t.Fatal("logging blocked on a full queue")
	}
	if stats := tel.LogStats(); stats.Dropped < 90 {
		t.Fatalf("expected drops, got %+v", stats)
	}
	close(exporter.block)
	shutdown(t, tel)
	if got := uint64(len(exporter.exported())) + tel.LogStats().Dropped; got != 104 {
		t.Fatalf("every record is either exported or counted as dropped: %d", got)
	}
	var dropped float64
	for _, metric := range tel.Metrics().Collect() {
		if metric.Name == "forge.telemetry.logs.dropped" {
			dropped = metric.Points[0].Value
		}
	}
	if dropped < 90 {
		t.Fatalf("dropped-log metric = %v", dropped)
	}
	// Logging after shutdown neither blocks nor panics.
	logger.Info("late")
}

func TestLogShutdownFlushesAndIsBoundedByItsDeadline(t *testing.T) {
	exporter := &recordingLogExporter{}
	tel := newTestTelemetry(t, Options{LogExporter: exporter, LogBatchTimeout: time.Hour})
	for range 30 {
		logsLogger(tel).Info("queued")
	}
	shutdown(t, tel)
	if got := len(exporter.exported()); got != 30 {
		t.Fatalf("shutdown must flush the queue: %d", got)
	}
	if exporter.shutdown != 1 {
		t.Fatalf("exporter shut down %d times", exporter.shutdown)
	}

	hung := &recordingLogExporter{block: make(chan struct{})}
	tel2, err := New(Options{ServiceName: "s", LogExporter: hung, LogBatchTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	logsLogger(tel2).Info("stuck")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = tel2.Shutdown(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("shutdown took %s with a 100ms budget", time.Since(start))
	}
}

func TestLogExportFailureCountsAndDoesNotRecurse(t *testing.T) {
	exporter := &recordingLogExporter{err: errors.New("collector down")}
	var diagnostics bytes.Buffer
	tel := newTestTelemetry(t, Options{LogExporter: exporter, Logger: slog.New(slog.NewJSONHandler(&diagnostics, nil))})
	logsLogger(tel).Info("one")
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := tel.LogStats(); stats.Failed != 1 || stats.Exported != 0 {
		t.Fatalf("stats %+v", stats)
	}
	if !strings.Contains(diagnostics.String(), `"msg":"telemetry: log export failed; log records dropped","log_records":1,`) {
		t.Fatalf("diagnostics: %q", diagnostics.String())
	}
	// The failure went to the diagnostics logger, not into the queue.
	if err := tel.ForceFlush(context.Background()); err != nil || tel.LogStats().Failed != 1 {
		t.Fatalf("export failure must not enqueue records: %+v", tel.LogStats())
	}
}

type secretHolder struct {
	Name   string
	Secret config.Secret
}

func TestLogExportNeverIncludesSecrets(t *testing.T) {
	c, server := newCollector(t)
	exporter := newExporter(t, server.URL, OTLPOptions{})
	tel := newTestTelemetry(t, Options{LogExporter: exporter})
	const secret = "hunter2-database-password"
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("postgres://app:" + secret + "@db/app")
	cfg.Telemetry.OTLPHeaders = config.NewSecret("authorization=" + secret)
	value := config.NewSecret(secret)

	logger := logsLogger(tel)
	logger.Info("direct", "token", value)
	logger.Info("pointer", "token", &value)
	logger.Info("struct", "holder", secretHolder{Name: "x", Secret: value})
	logger.Info("database", "config", cfg.Database)
	logger.Info("telemetry", "config", cfg.Telemetry)
	logger.With("token", value).WithGroup("g").Info("with", slog.Group("inner", "token", value))
	shutdown(t, tel)

	requests := c.received()
	if len(requests) == 0 {
		t.Fatal("nothing exported")
	}
	for _, request := range requests {
		encoded, _ := json.Marshal(request.payload)
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret exported: %s", encoded)
		}
		if !strings.Contains(string(encoded), "[REDACTED]") {
			t.Fatalf("expected the redaction marker: %s", encoded)
		}
	}
}

func TestTeeHandlerDispatchesCopiesToEnabledHandlers(t *testing.T) {
	var first, second bytes.Buffer
	info := slog.NewTextHandler(&first, &slog.HandlerOptions{Level: slog.LevelInfo})
	debug := slog.NewTextHandler(&second, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewTeeHandler(info, nil, debug)).With("a", 1).WithGroup("g")
	logger.Debug("d", "b", 2)
	logger.Info("i")
	if strings.Contains(first.String(), "msg=d") || !strings.Contains(second.String(), "msg=d") || !strings.Contains(second.String(), "a=1") || !strings.Contains(second.String(), "g.b=2") {
		t.Fatalf("first %q second %q", first.String(), second.String())
	}
	if !strings.Contains(first.String(), "msg=i") || !strings.Contains(second.String(), "msg=i") {
		t.Fatalf("first %q second %q", first.String(), second.String())
	}
	if NewTeeHandler(info) != info {
		t.Fatal("a single handler is returned unchanged")
	}
	if NewTeeHandler(nil).Enabled(context.Background(), slog.LevelError) {
		t.Fatal("no handlers means nothing is enabled")
	}
}

func TestFromConfigLogExporterIsOptIn(t *testing.T) {
	cfg := config.Default()
	cfg.Telemetry.ServiceName = "svc"
	tel, err := FromConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tel.LogHandler() != nil {
		t.Fatal("logs must be off by default")
	}
	shutdown(t, tel)

	_, server := newCollector(t)
	cfg.Telemetry.LogsExporter = config.TelemetryLogsExporterOTLP
	cfg.Telemetry.OTLPEndpoint = server.URL
	cfg.Telemetry.LogsLevel = "warn"
	tel, err = FromConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, tel)
	handler := tel.LogHandler()
	if handler == nil || handler.Enabled(context.Background(), slog.LevelInfo) || !handler.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("OTLP logs must be on with the configured level")
	}
	if tel.Stats() != (Stats{}) || tel.spans != nil {
		t.Fatal("logs-only configuration must not start a span exporter")
	}
}

// Values that standard output's slog.JSONHandler hides or redacts. Each
// carries logValueSecret somewhere the JSON encoding leaves out.
const logValueSecret = "zz-hunter2-qq"

type hiddenFields struct {
	Name     string
	Password string `json:"-"`
	token    string
}

type redactingJSON struct{ Token string }

func (redactingJSON) MarshalJSON() ([]byte, error) { return []byte(`"[hidden]"`), nil }

type redactingText struct{ Token string }

func (redactingText) MarshalText() ([]byte, error) { return []byte("[hidden]"), nil }

// redactingString hides itself from fmt but not from encoding/json.
type redactingString string

func (redactingString) String() string { return "[hidden]" }

type failingJSON struct{}

func (failingJSON) MarshalJSON() ([]byte, error) { return nil, errors.New(logValueSecret) }

type panickingJSON struct{}

func (panickingJSON) MarshalJSON() ([]byte, error) { panic(logValueSecret) }

func hiddenLogValues(t *testing.T) []slog.Attr {
	t.Helper()
	dsn, err := url.Parse("postgres://app:" + logValueSecret + "@db/app")
	if err != nil {
		t.Fatal(err)
	}
	return []slog.Attr{
		slog.Any("fields", hiddenFields{Name: "n", Password: logValueSecret, token: logValueSecret}),
		slog.Any("fields_ptr", &hiddenFields{Name: "n", Password: logValueSecret, token: logValueSecret}),
		slog.Any("url", dsn),
		slog.Any("marshal_json", redactingJSON{Token: logValueSecret}),
		slog.Any("marshal_text", redactingText{Token: logValueSecret}),
		slog.Any("error", errors.New("card declined")),
		slog.Any("map", map[string]any{"ok": true, "n": 2}),
		slog.Any("failing", failingJSON{}),
		slog.Any("panicking", panickingJSON{}),
		slog.Any("nil", nil),
	}
}

// exportedLogValues logs attrs through LogHandler with format and returns
// the exported attribute values by key.
func exportedLogValues(t *testing.T, format LogFormat, attrs []slog.Attr) map[string]string {
	t.Helper()
	exporter := &recordingLogExporter{}
	tel := newTestTelemetry(t, Options{LogExporter: exporter, LogFormat: format})
	logsLogger(tel).LogAttrs(context.Background(), slog.LevelInfo, "values", attrs...)
	shutdown(t, tel)
	records := exporter.exported()
	if len(records) != 1 {
		t.Fatalf("records: %+v", records)
	}
	return attrMap(records[0])
}

func TestLogExportEncodesValuesLikeTheJSONHandler(t *testing.T) {
	attrs := hiddenLogValues(t)
	exported := exportedLogValues(t, LogFormatJSON, attrs)

	var stdout bytes.Buffer
	slog.New(slog.NewJSONHandler(&stdout, nil)).LogAttrs(context.Background(), slog.LevelInfo, "values", attrs...)
	var line map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &line); err != nil {
		t.Fatalf("standard output %q: %v", stdout.String(), err)
	}
	for _, attr := range attrs {
		got, ok := exported[attr.Key]
		if !ok {
			t.Fatalf("%s not exported: %v", attr.Key, exported)
		}
		if strings.Contains(got, logValueSecret) {
			t.Fatalf("%s exported the secret: %q", attr.Key, got)
		}
		raw := line[attr.Key]
		if strings.Contains(string(raw), logValueSecret) {
			continue // slog's error text; checked below to stay out of the export
		}
		want := string(raw)
		var text string
		if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil {
			want = text
		}
		if attr.Key == "failing" || attr.Key == "panicking" {
			if !strings.HasPrefix(got, "!") || !strings.HasPrefix(want, "!") {
				t.Fatalf("%s: exported %q, standard output %q", attr.Key, got, want)
			}
			continue
		}
		if got != want {
			t.Fatalf("%s: exported %q, standard output %q", attr.Key, got, want)
		}
	}
	for key, want := range map[string]string{
		"fields":       `{"Name":"n"}`,
		"url":          "",
		"marshal_json": "[hidden]",
		"marshal_text": "[hidden]",
		"error":        "card declined",
		"failing":      "!ERROR: cannot encode telemetry.failingJSON",
		"panicking":    "!PANIC: encoding telemetry.panickingJSON",
		"nil":          "null",
	} {
		if want != "" && exported[key] != want {
			t.Fatalf("%s = %q, want %q", key, exported[key], want)
		}
	}
	if !strings.Contains(exported["url"], `"Host":"db"`) || !strings.Contains(exported["url"], `"User":{}`) {
		t.Fatalf("url = %q", exported["url"])
	}
}

func TestLogExportEncodesValuesLikeTheTextHandler(t *testing.T) {
	attrs := append(hiddenLogValues(t), slog.Any("stringer", redactingString(logValueSecret)), slog.Any("bytes", []byte("raw")))
	exported := exportedLogValues(t, LogFormatText, attrs)
	for _, attr := range attrs {
		var stdout bytes.Buffer
		drop := func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == slog.LevelKey || a.Key == slog.MessageKey {
				return slog.Attr{}
			}
			return a
		}
		slog.New(slog.NewTextHandler(&stdout, &slog.HandlerOptions{ReplaceAttr: drop})).LogAttrs(context.Background(), slog.LevelInfo, "", attr)
		want := strings.TrimSuffix(strings.TrimPrefix(stdout.String(), attr.Key+"="), "\n")
		if unquoted, err := strconv.Unquote(want); err == nil {
			want = unquoted
		}
		got := exported[attr.Key]
		switch attr.Key {
		case "url":
			// The text handler prints a URL with its password; the
			// exporter shows what standard output shows, no more.
			if got != want {
				t.Fatalf("url: exported %q, standard output %q", got, want)
			}
			continue
		}
		if strings.Contains(got, logValueSecret) && !strings.Contains(want, logValueSecret) {
			t.Fatalf("%s exported %q, more than standard output %q", attr.Key, got, want)
		}
		if got != want {
			t.Fatalf("%s: exported %q, standard output %q", attr.Key, got, want)
		}
	}
	if exported["stringer"] != "[hidden]" || exported["marshal_text"] != "[hidden]" || exported["error"] != "card declined" || exported["bytes"] != "raw" {
		t.Fatalf("exported %v", exported)
	}
}

func TestFromConfigMatchesTheStandardOutputFormat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		logger *slog.Logger
		want   LogFormat
	}{
		{"nil", nil, LogFormatJSON},
		{"json", slog.New(NewLogHandler(slog.NewJSONHandler(io.Discard, nil))), LogFormatJSON},
		{"text", slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil))), LogFormatText},
		{"bare text", slog.New(slog.NewTextHandler(io.Discard, nil)).With("a", 1), LogFormatText},
		{"other", slog.New(slog.DiscardHandler), LogFormatJSON},
	} {
		if got := logFormatOf(tc.logger); got != tc.want {
			t.Errorf("%s: format %d, want %d", tc.name, got, tc.want)
		}
	}
	if _, err := New(Options{ServiceName: "x", LogFormat: LogFormatText + 1}); err == nil {
		t.Fatal("an unknown log format must be rejected")
	}
}
