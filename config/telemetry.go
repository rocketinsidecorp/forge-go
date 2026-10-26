package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TelemetryExporter selects where sampled spans and pushed metrics go.
type TelemetryExporter string

const (
	// TelemetryExporterNone exports nothing: traces still correlate logs and
	// propagate to downstream services, and metrics are served by the
	// metrics listener when FORGE_TELEMETRY_METRICS_ADDR is set.
	TelemetryExporterNone TelemetryExporter = "none"
	// TelemetryExporterOTLP sends spans and metrics to an OpenTelemetry
	// Collector over OTLP/HTTP with JSON encoding.
	TelemetryExporterOTLP TelemetryExporter = "otlp"
	// TelemetryExporterLog writes spans to the application log. It is for
	// development and refused in production.
	TelemetryExporterLog TelemetryExporter = "log"
)

// TelemetryLogsExporter selects where application log records go in
// addition to standard output.
type TelemetryLogsExporter string

const (
	// TelemetryLogsExporterNone (the default) leaves logs on standard output.
	TelemetryLogsExporterNone TelemetryLogsExporter = "none"
	// TelemetryLogsExporterOTLP also sends every log record, with the trace
	// and span IDs of its context, to the OpenTelemetry Collector's
	// /v1/logs over OTLP/HTTP with JSON encoding, using the same
	// FORGE_TELEMETRY_OTLP_* settings as spans and metrics.
	TelemetryLogsExporterOTLP TelemetryLogsExporter = "otlp"
)

const (
	defaultOTLPTimeout     = 10 * time.Second
	maxOTLPTimeout         = time.Minute
	defaultMetricsInterval = time.Minute
	minMetricsInterval     = time.Second
	maxServiceNameLength   = 255
)

// Telemetry configures tracing, metrics and log correlation (package
// telemetry, docs/OBSERVABILITY.md).
type Telemetry struct {
	// Enabled turns instrumentation on (FORGE_TELEMETRY_ENABLED, default
	// true). When false no telemetry middleware or instrument exists and
	// the cost is zero.
	Enabled bool
	// ServiceName is the service.name resource attribute. Empty means
	// "unknown_service:" followed by the executable name.
	ServiceName    string
	ServiceVersion string
	// Exporter is none (default), otlp or log.
	Exporter TelemetryExporter
	// LogsExporter is none (default) or otlp
	// (FORGE_TELEMETRY_LOGS_EXPORTER). It is independent of Exporter: logs
	// may go to the collector while spans and metrics do not, and the other
	// way round. otlp needs OTLPEndpoint.
	LogsExporter TelemetryLogsExporter
	// LogsLevel is the lowest slog level sent to the log exporter:
	// debug, info (default), warn or error (FORGE_TELEMETRY_LOGS_LEVEL).
	// Standard output keeps the application logger's own level.
	LogsLevel string
	// OTLPEndpoint is the collector's base URL, such as
	// http://otel-collector:4318. Required for the otlp exporters. Production
	// requires https unless OTLPAllowPlaintext is set.
	OTLPEndpoint string
	// OTLPHeaders are "name=value" pairs separated by commas, values
	// percent-encoded, sent with every export: typically an API key.
	OTLPHeaders        Secret
	OTLPTimeout        time.Duration
	OTLPCompression    string
	OTLPAllowPlaintext bool
	// SampleRatio is the fraction of new traces that are sampled, 0 to 1
	// (default 1). Child spans follow their parent's decision.
	SampleRatio float64
	// TrustTraceparent continues traces from incoming traceparent and
	// tracestate headers (FORGE_TELEMETRY_TRUST_TRACEPARENT, default false).
	// Untrusted, every request starts a new trace, so clients can neither
	// choose trace IDs, force sampling past SampleRatio, nor push tracestate
	// to downstream services. Enable it only behind a proxy or gateway that
	// sets or sanitizes these headers.
	TrustTraceparent bool
	// MetricsAddress is the host:port of the separate Prometheus listener,
	// serving GET /metrics. Empty (default) means no listener. It has no
	// authentication: a non-loopback address must be confirmed with
	// MetricsPublic, or production refuses to start and other environments
	// log a warning.
	MetricsAddress string
	// MetricsPublic (FORGE_TELEMETRY_METRICS_PUBLIC) confirms that a
	// non-loopback MetricsAddress is intended, for example 0.0.0.0:9464 in a
	// container whose port only the cluster network can reach.
	MetricsPublic bool
	// MetricsInterval is how often metrics are pushed with the otlp
	// exporter (default 1 minute).
	MetricsInterval time.Duration
}

func defaultTelemetry() Telemetry {
	return Telemetry{
		Enabled:          true,
		Exporter:         TelemetryExporterNone,
		LogsExporter:     TelemetryLogsExporterNone,
		LogsLevel:        "info",
		OTLPTimeout:      defaultOTLPTimeout,
		OTLPCompression:  "none",
		SampleRatio:      1,
		TrustTraceparent: false,
		MetricsInterval:  defaultMetricsInterval,
	}
}

func loadTelemetry(lookup func(string) (string, bool), cfg *Telemetry) error {
	setString(lookup, "FORGE_TELEMETRY_SERVICE_NAME", &cfg.ServiceName)
	setString(lookup, "FORGE_TELEMETRY_SERVICE_VERSION", &cfg.ServiceVersion)
	setString(lookup, "FORGE_TELEMETRY_OTLP_ENDPOINT", &cfg.OTLPEndpoint)
	setString(lookup, "FORGE_TELEMETRY_OTLP_COMPRESSION", &cfg.OTLPCompression)
	setString(lookup, "FORGE_TELEMETRY_METRICS_ADDR", &cfg.MetricsAddress)
	setString(lookup, "FORGE_TELEMETRY_LOGS_LEVEL", &cfg.LogsLevel)
	if value, ok := lookup("FORGE_TELEMETRY_LOGS_EXPORTER"); ok {
		cfg.LogsExporter = TelemetryLogsExporter(value)
	}
	if value, ok := lookup("FORGE_TELEMETRY_EXPORTER"); ok {
		cfg.Exporter = TelemetryExporter(value)
	}
	if value, ok := lookup("FORGE_TELEMETRY_OTLP_HEADERS"); ok {
		cfg.OTLPHeaders = NewSecret(value)
	}
	for key, target := range map[string]*bool{
		"FORGE_TELEMETRY_ENABLED":              &cfg.Enabled,
		"FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT": &cfg.OTLPAllowPlaintext,
		"FORGE_TELEMETRY_TRUST_TRACEPARENT":    &cfg.TrustTraceparent,
		"FORGE_TELEMETRY_METRICS_PUBLIC":       &cfg.MetricsPublic,
	} {
		if value, ok := lookup(key); ok {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return invalidValue(key, "true or false")
			}
			*target = parsed
		}
	}
	for key, target := range map[string]*time.Duration{
		"FORGE_TELEMETRY_OTLP_TIMEOUT":     &cfg.OTLPTimeout,
		"FORGE_TELEMETRY_METRICS_INTERVAL": &cfg.MetricsInterval,
	} {
		if value, ok := lookup(key); ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return invalidValue(key, "a duration such as 10s")
			}
			*target = parsed
		}
	}
	if value, ok := lookup("FORGE_TELEMETRY_SAMPLE_RATIO"); ok {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return invalidValue("FORGE_TELEMETRY_SAMPLE_RATIO", "a number between 0 and 1")
		}
		cfg.SampleRatio = parsed
	}
	return nil
}

func (t Telemetry) validate(environment Environment, httpAddress string) error {
	if !t.Enabled {
		return nil
	}
	if len(t.ServiceName) > maxServiceNameLength || len(t.ServiceVersion) > maxServiceNameLength {
		return fmt.Errorf("FORGE_TELEMETRY_SERVICE_NAME and FORGE_TELEMETRY_SERVICE_VERSION must be at most %d bytes", maxServiceNameLength)
	}
	if math.IsNaN(t.SampleRatio) || t.SampleRatio < 0 || t.SampleRatio > 1 {
		return errors.New("FORGE_TELEMETRY_SAMPLE_RATIO must be between 0 and 1")
	}
	if t.MetricsInterval < minMetricsInterval {
		return fmt.Errorf("FORGE_TELEMETRY_METRICS_INTERVAL must be at least %s", minMetricsInterval)
	}
	if t.MetricsAddress != "" {
		host, port, err := net.SplitHostPort(t.MetricsAddress)
		if number, convErr := strconv.Atoi(port); err != nil || convErr != nil || number < 0 || number > 65535 || strings.ContainsAny(host, "/?#@") {
			return errors.New("FORGE_TELEMETRY_METRICS_ADDR must be host:port, such as 127.0.0.1:9464")
		}
		if t.MetricsAddress == httpAddress {
			return errors.New("FORGE_TELEMETRY_METRICS_ADDR must differ from FORGE_HTTP_ADDR: metrics are never served on the application listener")
		}
		if environment == Production && !t.MetricsLoopback() && !t.MetricsPublic {
			return errors.New("FORGE_TELEMETRY_METRICS_ADDR is not a loopback address and the metrics listener has no authentication: bind it to 127.0.0.1, or set FORGE_TELEMETRY_METRICS_PUBLIC=true when only a private network can reach it")
		}
	}
	if _, ok := t.LogLevel(); !ok {
		return fmt.Errorf("unsupported FORGE_TELEMETRY_LOGS_LEVEL %q: expected debug, info, warn or error", t.LogsLevel)
	}
	needsOTLP := false
	switch t.Exporter {
	case TelemetryExporterNone:
	case TelemetryExporterLog:
		if environment == Production {
			return errors.New("FORGE_TELEMETRY_EXPORTER=log is for development and cannot be used in production")
		}
	case TelemetryExporterOTLP:
		needsOTLP = true
	default:
		return fmt.Errorf("unsupported FORGE_TELEMETRY_EXPORTER %q: expected none, otlp or log", t.Exporter)
	}
	switch t.LogsExporter {
	case TelemetryLogsExporterNone:
	case TelemetryLogsExporterOTLP:
		needsOTLP = true
	default:
		return fmt.Errorf("unsupported FORGE_TELEMETRY_LOGS_EXPORTER %q: expected none or otlp", t.LogsExporter)
	}
	if needsOTLP {
		return t.validateOTLP(environment)
	}
	return nil
}

// LogLevel returns the slog level LogsLevel names, and false when it names
// none.
func (t Telemetry) LogLevel() (slog.Level, bool) {
	switch strings.ToLower(t.LogsLevel) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

func (t Telemetry) validateOTLP(environment Environment) error {
	if t.OTLPEndpoint == "" {
		return errors.New("FORGE_TELEMETRY_EXPORTER=otlp and FORGE_TELEMETRY_LOGS_EXPORTER=otlp require FORGE_TELEMETRY_OTLP_ENDPOINT")
	}
	// Errors never quote the endpoint or headers.
	parsed, err := url.Parse(t.OTLPEndpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return errors.New("FORGE_TELEMETRY_OTLP_ENDPOINT must be an http:// or https:// URL without credentials, query or fragment")
	}
	if port := parsed.Port(); port != "" {
		if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
			return errors.New("FORGE_TELEMETRY_OTLP_ENDPOINT has an invalid port")
		}
	}
	if environment == Production && parsed.Scheme != "https" && !t.OTLPAllowPlaintext {
		return errors.New("production requires an https FORGE_TELEMETRY_OTLP_ENDPOINT unless FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT=true")
	}
	if t.OTLPTimeout <= 0 || t.OTLPTimeout > maxOTLPTimeout {
		return fmt.Errorf("FORGE_TELEMETRY_OTLP_TIMEOUT must be positive and at most %s", maxOTLPTimeout)
	}
	if t.OTLPCompression != "none" && t.OTLPCompression != "gzip" {
		return errors.New("FORGE_TELEMETRY_OTLP_COMPRESSION must be none or gzip")
	}
	_, err = t.OTLPHeaderMap()
	return err
}

// MetricsLoopback reports whether MetricsAddress binds only to loopback
// (localhost, 127.0.0.0/8 or ::1). An empty host, 0.0.0.0 and any other
// address are reachable from the network.
func (t Telemetry) MetricsLoopback() bool {
	host, _, err := net.SplitHostPort(t.MetricsAddress)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// OTLPHeaderMap parses OTLPHeaders: "name1=value1,name2=value2" with
// percent-encoded values, the OTEL_EXPORTER_OTLP_HEADERS format. Errors name
// the position of the bad pair, never a value.
func (t Telemetry) OTLPHeaderMap() (map[string]string, error) {
	headers := make(map[string]string)
	raw := t.OTLPHeaders.Reveal()
	if strings.TrimSpace(raw) == "" {
		return headers, nil
	}
	for i, item := range strings.Split(raw, ",") {
		name, value, found := strings.Cut(item, "=")
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !found || !validHeaderName(name) {
			return nil, fmt.Errorf("FORGE_TELEMETRY_OTLP_HEADERS entry %d must be name=value with a valid header name", i+1)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil || strings.ContainsFunc(decoded, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) {
			return nil, fmt.Errorf("FORGE_TELEMETRY_OTLP_HEADERS entry %d has an invalid value", i+1)
		}
		switch name {
		case "Content-Type", "Content-Encoding", "Content-Length", "Host", "Transfer-Encoding", "Connection":
			return nil, fmt.Errorf("FORGE_TELEMETRY_OTLP_HEADERS entry %d sets a reserved header", i+1)
		}
		if _, duplicate := headers[name]; duplicate {
			return nil, fmt.Errorf("FORGE_TELEMETRY_OTLP_HEADERS entry %d repeats a header", i+1)
		}
		headers[name] = decoded
	}
	return headers, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c <= 0x20 || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}
