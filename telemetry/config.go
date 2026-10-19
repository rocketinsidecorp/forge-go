package telemetry

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rocketinsidecorp/forge-go/config"
)

// FromConfig builds the Telemetry that cfg.Telemetry describes (the
// FORGE_TELEMETRY_* variables), or returns nil when it is disabled. logger
// receives export failures and, with the log exporter, the spans. The caller
// owns the result and must call Shutdown; forge.App does both.
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
	switch settings.Exporter {
	case config.TelemetryExporterLog:
		opts.SpanExporter = NewLogExporter(logger)
	case config.TelemetryExporterOTLP:
		headers, err := settings.OTLPHeaderMap()
		if err != nil {
			return nil, err
		}
		exporter, err := NewOTLPExporter(OTLPOptions{
			Endpoint: settings.OTLPEndpoint,
			Headers:  headers,
			Gzip:     settings.OTLPCompression == "gzip",
		})
		if err != nil {
			return nil, err
		}
		opts.SpanExporter = exporter
		opts.MetricExporter = exporter
	}
	return New(opts)
}
