package telemetry

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rocketinsidecorp/forge-go/config"
)

// FromConfig builds the Telemetry that cfg.Telemetry describes (the
// FORGE_TELEMETRY_* variables), or returns nil when it is disabled. logger
// receives export failures and, with the log exporter, the spans; it must
// write to standard output only, never to the telemetry's own log handler,
// or a failed log export would log and export again. The caller owns the
// result and must call Shutdown; forge.App does both. With
// FORGE_TELEMETRY_LOGS_EXPORTER=otlp the result's LogHandler is non-nil and
// forge.App tees its logger into it; the exporter then encodes and rewrites
// attributes as logger's handler does: Options.LogFormat and
// Options.LogReplaceAttr come from its LogOutput when it is a
// LogOutputHandler (NewOutputHandler's and NewLogHandler's results are), text
// for a bare slog.TextHandler, and JSON without ReplaceAttr otherwise.
func FromConfig(cfg config.Config, logger *slog.Logger) (*Telemetry, error) {
	settings := cfg.Telemetry
	if !settings.Enabled {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	name := settings.ServiceName
	if name == "" {
		name = "unknown_service:" + filepath.Base(os.Args[0])
	}
	opts := Options{
		ServiceName:     name,
		ServiceVersion:  settings.ServiceVersion,
		Environment:     string(cfg.Environment),
		SampleRatio:     settings.SampleRatio,
		TrustIncoming:   settings.TrustTraceparent,
		MetricsInterval: settings.MetricsInterval,
		ExportTimeout:   settings.OTLPTimeout,
		Logger:          logger,
	}
	if logger != nil {
		// Statement lines go to standard output only, never to the log
		// exporter: they are for the developer watching, or for an operator
		// who set FORGE_LOG_LEVEL=debug.
		opts.QueryLog = &QueryLog{Logger: logger, Level: cfg.EventLevel(), Slow: cfg.Log.SlowQuery}
	}
	if logger != nil {
		output := logOutputOf(logger.Handler())
		opts.LogFormat, opts.LogReplaceAttr = output.Format, output.ReplaceAttr
	}
	var otlp *OTLPExporter
	if settings.Exporter == config.TelemetryExporterOTLP || settings.LogsExporter == config.TelemetryLogsExporterOTLP {
		headers, err := settings.OTLPHeaderMap()
		if err != nil {
			return nil, err
		}
		otlp, err = NewOTLPExporter(OTLPOptions{
			Endpoint: settings.OTLPEndpoint,
			Headers:  headers,
			Gzip:     settings.OTLPCompression == "gzip",
		})
		if err != nil {
			return nil, err
		}
	}
	switch settings.Exporter {
	case config.TelemetryExporterLog:
		opts.SpanExporter = NewLogExporter(logger)
	case config.TelemetryExporterOTLP:
		opts.SpanExporter = otlp
		opts.MetricExporter = otlp
	}
	if settings.LogsExporter == config.TelemetryLogsExporterOTLP {
		opts.LogExporter = otlp
		opts.LogLevel, _ = settings.LogLevel()
	}
	return New(opts)
}
