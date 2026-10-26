package config

import (
	"fmt"
	"log/slog"
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
		got.MetricsAddress != "" || got.LogsExporter != TelemetryLogsExporterNone || got.LogsLevel != "info" || got.OTLPTimeout != 10*time.Second || got.MetricsInterval != time.Minute {
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
		"bad bool":              {"FORGE_TELEMETRY_ENABLED": "maybe"},
		"bad ratio":             {"FORGE_TELEMETRY_SAMPLE_RATIO": "1.5"},
		"ratio not a number":    {"FORGE_TELEMETRY_SAMPLE_RATIO": "half"},
		"nan ratio":             {"FORGE_TELEMETRY_SAMPLE_RATIO": "NaN"},
		"unknown exporter":      {"FORGE_TELEMETRY_EXPORTER": "zipkin"},
		"otlp no endpoint":      {"FORGE_TELEMETRY_EXPORTER": "otlp"},
		"endpoint userinfo":     {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://user:" + secret + "@collector"},
		"endpoint query":        {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://collector/?token=" + secret},
		"endpoint scheme":       {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "file:///etc/passwd"},
		"endpoint port":         {"FORGE_TELEMETRY_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://collector:0"},
		"compression":           merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_COMPRESSION": "zstd"}),
		"timeout":               merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_TIMEOUT": "0s"}),
		"header format":         merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": secret}),
		"header reserved":       merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": "Host=" + secret}),
		"header control":        merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": "x-key=a%0D%0Ab" + secret}),
		"header duplicate":      merge(base, map[string]string{"FORGE_TELEMETRY_OTLP_HEADERS": "a=1,A=" + secret}),
		"metrics addr":          {"FORGE_TELEMETRY_METRICS_ADDR": "9464"},
		"metrics addr reused":   {"FORGE_HTTP_ADDR": "127.0.0.1:8080", "FORGE_TELEMETRY_METRICS_ADDR": "127.0.0.1:8080"},
		"metrics interval":      {"FORGE_TELEMETRY_METRICS_INTERVAL": "10ms"},
		"plaintext prod":        production,
		"log exporter prod":     {"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_TELEMETRY_EXPORTER": "log"},
		"long service name":     {"FORGE_TELEMETRY_SERVICE_NAME": strings.Repeat("s", 256)},
		"public metrics prod":   {"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_TELEMETRY_METRICS_ADDR": "0.0.0.0:9464"},
		"metrics all ifaces":    {"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_TELEMETRY_METRICS_ADDR": ":9464"},
		"bad metrics public":    {"FORGE_TELEMETRY_METRICS_PUBLIC": "yes please"},
		"unknown logs exporter": {"FORGE_TELEMETRY_LOGS_EXPORTER": "loki"},
		"logs otlp no endpoint": {"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp"},
		"logs plaintext prod":   {"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_TELEMETRY_LOGS_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "http://collector:4318"},
		"logs bad header":       {"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://collector", "FORGE_TELEMETRY_OTLP_HEADERS": secret},
		"logs bad level":        {"FORGE_TELEMETRY_LOGS_LEVEL": "chatty"},
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
	// Port 0 asks the kernel for a free port; the bound address is logged.
	if _, err := LoadWithLookup(lookupFrom(merge(prodMetrics, map[string]string{"FORGE_TELEMETRY_METRICS_ADDR": "127.0.0.1:0"}))); err != nil {
		t.Fatalf("metrics port 0 rejected: %v", err)
	}
	// Two kernel-chosen ports never collide, so the same spelling with port
	// 0 is accepted, while the same fixed port is still refused.
	for _, address := range []string{"127.0.0.1:0", ":0", "localhost:0"} {
		same := map[string]string{"FORGE_HTTP_ADDR": address, "FORGE_TELEMETRY_METRICS_ADDR": address}
		if _, err := LoadWithLookup(lookupFrom(same)); err != nil {
			t.Fatalf("HTTP and metrics both on %s rejected: %v", address, err)
		}
	}
	for _, pair := range [][2]string{
		{"127.0.0.1:9464", "127.0.0.1:9464"},
		{"0.0.0.0:9464", "127.0.0.1:9464"},
		{":9464", "localhost:9464"},
		{"127.0.0.1:9464", "[::]:9464"},
	} {
		reused := map[string]string{"FORGE_HTTP_ADDR": pair[0], "FORGE_TELEMETRY_METRICS_ADDR": pair[1]}
		if _, err := LoadWithLookup(lookupFrom(reused)); err == nil || !strings.Contains(err.Error(), "must differ from FORGE_HTTP_ADDR") {
			t.Fatalf("HTTP %s and metrics %s were accepted: %v", pair[0], pair[1], err)
		}
	}
	separate := map[string]string{"FORGE_HTTP_ADDR": "127.0.0.1:9464", "FORGE_TELEMETRY_METRICS_ADDR": "127.0.0.2:9464"}
	if _, err := LoadWithLookup(lookupFrom(separate)); err != nil {
		t.Fatalf("different explicit hosts on one port rejected: %v", err)
	}
	public := merge(prodMetrics, map[string]string{"FORGE_TELEMETRY_METRICS_ADDR": "0.0.0.0:9464", "FORGE_TELEMETRY_METRICS_PUBLIC": "true"})
	if _, err := LoadWithLookup(lookupFrom(public)); err != nil {
		t.Fatalf("explicit public metrics opt-in rejected: %v", err)
	}
	logs := map[string]string{"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ENDPOINT": "https://collector.example.com", "FORGE_TELEMETRY_LOGS_LEVEL": "WARN"}
	cfg, err := LoadWithLookup(lookupFrom(logs))
	if err != nil {
		t.Fatalf("logs exporter without the trace exporter rejected: %v", err)
	}
	if level, ok := cfg.Telemetry.LogLevel(); !ok || level != slog.LevelWarn || cfg.Telemetry.Exporter != TelemetryExporterNone || cfg.Telemetry.LogsExporter != TelemetryLogsExporterOTLP {
		t.Fatalf("logs settings %+v", cfg.Telemetry)
	}
	if _, err := LoadWithLookup(lookupFrom(merge(production, map[string]string{"FORGE_TELEMETRY_LOGS_EXPORTER": "otlp", "FORGE_TELEMETRY_OTLP_ALLOW_PLAINTEXT": "true"}))); err != nil {
		t.Fatalf("plaintext opt-in rejected for logs: %v", err)
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

// TestListenAddressesOverlap mirrors Forge-Rust's metrics/HTTP address rule:
// the same non-zero port with the same host or an unspecified host on either
// side overlaps; port 0 and different explicit hosts never do.
func TestListenAddressesOverlap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:8080", "127.0.0.1:8080", true},
		{"0.0.0.0:8080", "127.0.0.1:8080", true},
		{"127.0.0.1:8080", "0.0.0.0:8080", true},
		{":8080", "localhost:8080", true},
		{"localhost:8080", ":8080", true},
		{"[::]:8080", "127.0.0.1:8080", true},
		{"[::]:8080", "[::1]:8080", true},
		{"[::]:8080", "0.0.0.0:8080", true},
		{"0.0.0.0:8080", "0.0.0.0:8080", true},
		{"[::1]:8080", "[0:0:0:0:0:0:0:1]:8080", true},
		{"localhost:8080", "LOCALHOST:8080", true},
		{"127.0.0.1:8080", "127.0.0.1:9464", false},
		{"127.0.0.1:8080", "127.0.0.2:8080", false},
		{"127.0.0.1:8080", "[::1]:8080", false},
		{"0.0.0.0:8080", "127.0.0.1:9464", false},
		{"127.0.0.1:0", "127.0.0.1:0", false},
		{":0", ":0", false},
		{"0.0.0.0:0", "127.0.0.1:0", false},
		{"[::]:0", "[::]:0", false},
		{"127.0.0.1:0", "127.0.0.1:8080", false},
		{"0.0.0.0:8080", "127.0.0.1:0", false},
		{"not an address", "not an address", true},
		{"not an address", "127.0.0.1:8080", false},
	}
	for _, c := range cases {
		if got := listenAddressesOverlap(c.a, c.b); got != c.want {
			t.Errorf("listenAddressesOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := listenAddressesOverlap(c.b, c.a); got != c.want {
			t.Errorf("listenAddressesOverlap(%q, %q) = %v, want %v (swapped)", c.b, c.a, got, c.want)
		}
	}
}
