package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTelemetryDefaults(t *testing.T) {
	cfg, err := LoadWithLookup(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Telemetry
	// Incoming trace context is untrusted unless explicitly enabled.
	if !got.Enabled || got.Exporter != TelemetryExporterNone || got.SampleRatio != 1 || got.TrustTraceparent || got.MetricsPublic ||
		got.MetricsAddress != "" || got.OTLPTimeout != 10*time.Second || got.MetricsInterval != time.Minute {
		t.Fatalf("defaults %+v", got)
	}
}

func TestTelemetryLoadsEnvironment(t *testing.T) {
	cfg, err := LoadWithLookup(lookupFrom(map[string]string{
		"FORGE_TELEMETRY_ENABLED":           "true",
		"FORGE_TELEMETRY_SERVICE_NAME":      "billing",
		"FORGE_TELEMETRY_SERVICE_VERSION":   "1.4.0",
		"FORGE_TELEMETRY_EXPORTER":          "otlp",
		"FORGE_TELEMETRY_OTLP_ENDPOINT":     "http://otel-collector:4318",
		"FORGE_TELEMETRY_OTLP_HEADERS":      "x-api-key=abc%3D,Authorization=Bearer%20t0k",
		"FORGE_TELEMETRY_OTLP_TIMEOUT":      "3s",
		"FORGE_TELEMETRY_OTLP_COMPRESSION":  "gzip",
		"FORGE_TELEMETRY_SAMPLE_RATIO":      "0.25",
		"FORGE_TELEMETRY_TRUST_TRACEPARENT": "true",
		"FORGE_TELEMETRY_METRICS_ADDR":      "0.0.0.0:9464",
		"FORGE_TELEMETRY_METRICS_PUBLIC":    "true",
		"FORGE_TELEMETRY_METRICS_INTERVAL":  "15s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Telemetry
	if got.ServiceName != "billing" || got.ServiceVersion != "1.4.0" || got.Exporter != TelemetryExporterOTLP ||
		got.OTLPTimeout != 3*time.Second || got.OTLPCompression != "gzip" || got.SampleRatio != 0.25 || !got.TrustTraceparent || !got.MetricsPublic ||
		got.MetricsAddress != "0.0.0.0:9464" || got.MetricsInterval != 15*time.Second {
		t.Fatalf("loaded %+v", got)
	}
	headers, err := got.OTLPHeaderMap()
	if err != nil || headers["X-Api-Key"] != "abc=" || headers["Authorization"] != "Bearer t0k" {
		t.Fatalf("headers %v %v", headers, err)
	}
	if printed := fmt.Sprintf("%v %+v", got, got); strings.Contains(printed, "t0k") || strings.Contains(printed, "abc") {
		t.Fatalf("headers leaked when printed: %s", printed)
	}
}

func TestTelemetryValidation(t *testing.T) {
	const secret = "sup3rs3cret"
	base := map[string]string{"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://collector.example.com"}
	production := map[string]string{
		"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy",
		"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "http://collector:4318",
	}
	cases := map[string]map[string]string{
		"bad bool":            {"FORGE_TELEMETRY_ENABLED": "maybe"},
		"bad ratio":           {"FORGE_TELEMETRY_SAMPLE_RATIO": "1.5"},
		"ratio not a number":  {"FORGE_TELEMETRY_SAMPLE_RATIO": "half"},
		"nan ratio":           {"FORGE_TELEMETRY_SAMPLE_RATIO": "NaN"},
		"unknown exporter":    {"FORGE_TELEMETRY_EXPORTER": "zipkin"},
		"otlp no endpoint":    {"FORGE_TELEMETRY_EXPORTER": "otlp"},
		"endpoint userinfo":   {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://user:" + secret + "@collector"},
		"endpoint query":      {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://collector/?token=" + secret},
		"endpoint scheme":     {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "file:///etc/passwd"},
		"endpoint port":       {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://collector:0"},
		"compression":         merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_COMPRESSION": "zstd"}),
		"timeout":             merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_TIMEOUT": "0s"}),
		"header format":       merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": secret}),
		"header reserved":     merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": "Host=" + secret}),
		"header control":      merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": "x-key=a%0D%0Ab" + secret}),
		"header duplicate":    merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": "a=1,A=" + secret}),
		"metrics addr":        {"FORGE_TELEMETRY_METRICS_ADDR": "9464"},
		"metrics addr reused": {"FORGE_HTTP_ADDR": "127.0.0.1:8080", "FORGE_TELEMETRY_METRICS_ADDR": "127.0.0.1:8080"},
		"metrics interval":    {"FORGE_TELEMETRY_METRICS_INTERVAL": "10ms"},
		"plaintext prod":      production,
		"log exporter prod":   {"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_TELEMETRY_EXPORTER": "log"},
		"long service name":   {"FORGE_TELEMETRY_SERVICE_NAME": strings.Repeat("s", 256)},
		"public metrics prod": {"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_TELEMETRY_METRICS_ADDR": "0.0.0.0:9464"},
		"metrics all ifaces":  {"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_TELEMETRY_METRICS_ADDR": ":9464"},
		"bad metrics public":  {"FORGE_TELEMETRY_METRICS_PUBLIC": "yes please"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadWithLookup(lookupFrom(env))
			if err == nil {
				t.Fatal("configuration accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error echoes a secret: %v", err)
			}
		})
	}

	allowed := merge(production, map[string]string{"FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT": "true"})
	if _, err := LoadWithLookup(lookupFrom(allowed)); err != nil {
		t.Fatalf("explicit plaintext opt-in rejected: %v", err)
	}
	prodMetrics := map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy"}
	for _, address := range []string{"127.0.0.1:9464", "localhost:9464", "[::1]:9464"} {
		if _, err := LoadWithLookup(lookupFrom(merge(prodMetrics, map[string]string{"FORGE_TELEMETRY_METRICS_ADDR": address}))); err != nil {
			t.Fatalf("loopback metrics address %s rejected: %v", address, err)
		}
	}
	public := merge(prodMetrics, map[string]string{"FORGE_TELEMETRY_METRICS_ADDR": "0.0.0.0:9464", "FORGE_TELEMETRY_METRICS_PUBLIC": "true"})
	if _, err := LoadWithLookup(lookupFrom(public)); err != nil {
		t.Fatalf("explicit public metrics opt-in rejected: %v", err)
	}
	disabled := map[string]string{"FORGE_TELEMETRY_ENABLED": "false", "FORGE_TELEMETRY_EXPORTER": "otlp"}
	if _, err := LoadWithLookup(lookupFrom(disabled)); err != nil {
		t.Fatalf("disabled telemetry is not validated further: %v", err)
	}
}

func merge(maps ...map[string]string) map[string]string {
	result := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}
