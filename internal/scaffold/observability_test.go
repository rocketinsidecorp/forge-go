package scaffold_test

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// observabilityFiles are the files `forge new` writes for the opt-in
// observability stack, relative to the application directory.
var observabilityFiles = []string{
	"compose.observability.yaml",
	"observability/otel-collector.yaml",
	"observability/prometheus.yml",
	"observability/alerts.yml",
	"observability/tempo.yaml",
	"observability/loki.yaml",
	"observability/grafana/provisioning/datasources/datasources.yaml",
	"observability/grafana/provisioning/dashboards/dashboards.yaml",
	"observability/grafana/dashboards/forge-application.json",
}

// stackServices are the services the overlay adds to compose.yaml.
var stackServices = []string{"otel-collector", "prometheus", "tempo", "loki", "grafana"}

var pinnedImage = regexp.MustCompile(`^[a-z0-9./-]+:v?\d+\.\d+\.\d+@sha256:[0-9a-f]{64}$`)

func generateObservability(t *testing.T, database string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir, Database: database}); err != nil {
		t.Fatalf("generate (database %q): %v", database, err)
	}
	return dir
}

func readYAML(t *testing.T, dir, relative string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "\t") {
		t.Fatalf("%s contains a tab; YAML indents with spaces", relative)
	}
	var document map[string]any
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatalf("%s is not valid YAML: %v", relative, err)
	}
	return document
}

func mapAt(t *testing.T, value any, path ...string) map[string]any {
	t.Helper()
	for _, key := range path {
		parent, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%v is not a mapping at %q", value, key)
		}
		if value, ok = parent[key]; !ok {
			t.Fatalf("missing key %q", key)
		}
	}
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is not a mapping", value)
	}
	return result
}

func stringList(t *testing.T, value any) []string {
	t.Helper()
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("list item %v is not a string", item)
		}
		result = append(result, text)
	}
	return result
}

func TestGenerateObservabilityStackForEveryDatabase(t *testing.T) {
	for _, database := range []string{"", "postgresql", "mysql", "mariadb", "sqlite"} {
		t.Run("database_"+database, func(t *testing.T) {
			dir := generateObservability(t, database)
			for _, relative := range observabilityFiles {
				info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(relative)))
				if err != nil {
					t.Fatalf("missing %s: %v", relative, err)
				}
				if strings.HasPrefix(relative, "observability/") && runtime.GOOS != "windows" {
					// Containers run as other users and read these bind mounts.
					if info.Mode().Perm()&0o044 != 0o044 {
						t.Errorf("%s is %v; containers running as other users cannot read it", relative, info.Mode().Perm())
					}
				}
			}
			for _, directory := range []string{"observability", "observability/grafana", "observability/grafana/provisioning/datasources"} {
				info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(directory)))
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm()&0o055 != 0o055 {
					t.Errorf("%s is %v; containers cannot traverse it", directory, info.Mode().Perm())
				}
			}
			// Every file is well-formed.
			for _, relative := range observabilityFiles {
				switch filepath.Ext(relative) {
				case ".yaml", ".yml":
					readYAML(t, dir, relative)
				case ".json":
					var document map[string]any
					content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(relative)))
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(content, &document); err != nil {
						t.Fatalf("%s is not valid JSON: %v", relative, err)
					}
				}
			}
		})
	}
}

func TestObservabilityOverlayIsOptInAndLoopbackOnly(t *testing.T) {
	dir := generateObservability(t, "postgresql")
	defaultCompose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range append([]string{"grafana", "prometheus", "tempo", "loki", "otel", "FORGE_TELEMETRY", "GRAFANA_ADMIN_PASSWORD", "fluentd"}, stackServices...) {
		if strings.Contains(string(defaultCompose), forbidden) {
			t.Errorf("the default compose.yaml mentions %q; the observability stack must stay opt-in", forbidden)
		}
	}

	overlay := readYAML(t, dir, "compose.observability.yaml")
	if _, ok := overlay["profiles"]; ok {
		t.Error("top-level profiles key is not valid compose")
	}
	services := mapAt(t, overlay, "services")
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	want := append([]string{"app"}, stackServices...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("overlay services = %v, want %v", names, want)
	}

	for _, name := range stackServices {
		service := mapAt(t, services, name)
		image, _ := service["image"].(string)
		if strings.Contains(image, "latest") || !pinnedImage.MatchString(image) {
			t.Errorf("%s image %q must be pinned to an exact version and digest", name, image)
		}
		for _, port := range stringList(t, service["ports"]) {
			if !strings.HasPrefix(port, "127.0.0.1:") {
				t.Errorf("%s publishes %q; every published port must bind 127.0.0.1", name, port)
			}
		}
		// Bind-mounted configuration must exist in the generated project.
		for _, volume := range stringList(t, service["volumes"]) {
			source, _, _ := strings.Cut(volume, ":")
			if strings.HasPrefix(source, "./") {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(source))); err != nil {
					t.Errorf("%s mounts %s: %v", name, source, err)
				}
				if !strings.HasSuffix(volume, ":ro") {
					t.Errorf("%s mounts %s read-write; configuration is read-only", name, volume)
				}
			}
		}
	}
	// Tempo, Loki and the collector's own listeners stay off the host: only
	// the collector (OTLP, log driver) and the two UIs are published.
	for _, name := range []string{"tempo", "loki"} {
		if _, ok := mapAt(t, services, name)["ports"]; ok {
			t.Errorf("%s must not publish ports", name)
		}
	}
}

func TestObservabilityGrafanaIsSecuredByDefault(t *testing.T) {
	dir := generateObservability(t, "sqlite")
	grafana := mapAt(t, readYAML(t, dir, "compose.observability.yaml"), "services", "grafana")
	environment := mapAt(t, grafana, "environment")
	password, _ := environment["GF_SECURITY_ADMIN_PASSWORD"].(string)
	if !strings.HasPrefix(password, "${GRAFANA_ADMIN_PASSWORD:?") {
		t.Errorf("admin password = %q; it must be required from GRAFANA_ADMIN_PASSWORD with no default", password)
	}
	for key, want := range map[string]string{
		"GF_AUTH_ANONYMOUS_ENABLED":             "false",
		"GF_USERS_ALLOW_SIGN_UP":                "false",
		"GF_ANALYTICS_REPORTING_ENABLED":        "false",
		"GF_ANALYTICS_CHECK_FOR_UPDATES":        "false",
		"GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES": "false",
		"GF_PLUGINS_PREINSTALL_DISABLED":        "true",
		"GF_NEWS_NEWS_FEED_ENABLED":             "false",
	} {
		if got, _ := environment[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// No committed secret anywhere in the stack's files.
	secret := regexp.MustCompile(`(?i)(password|secret|token|api[_-]?key)\s*[:=]\s*["']?[^\s"'$#]`)
	for _, relative := range observabilityFiles {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(content), "\n") {
			// Comments and ${VAR} references carry no value.
			if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.Contains(line, "${") {
				continue
			}
			if secret.MatchString(line) {
				t.Errorf("%s may contain a secret: %q", relative, line)
			}
		}
	}
}

func TestObservabilityAppSettingsAreValidFrameworkConfiguration(t *testing.T) {
	dir := generateObservability(t, "postgresql")
	app := mapAt(t, readYAML(t, dir, "compose.observability.yaml"), "services", "app")
	environment := map[string]string{
		"FORGE_ENV":            "development",
		"FORGE_HTTP_ADDR":      "0.0.0.0:8080",
		"FORGE_HTTP_TRANSPORT": "plain",
		"FORGE_DATABASE_URL":   "postgres://shop:development@postgres:5432/shop?sslmode=disable",
	}
	for key, value := range mapAt(t, app, "environment") {
		text, _ := value.(string)
		environment[key] = text
	}
	cfg, err := config.LoadWithLookup(func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("the overlay's app environment is not valid Forge configuration: %v", err)
	}
	if cfg.Telemetry.Exporter != config.TelemetryExporterOTLP || cfg.Telemetry.ServiceName != "shop" {
		t.Errorf("telemetry = %+v; want the otlp exporter and service name shop", cfg.Telemetry)
	}
	endpoint, err := url.Parse(cfg.Telemetry.OTLPEndpoint)
	if err != nil || endpoint.Hostname() != "otel-collector" || endpoint.Port() != "4318" {
		t.Errorf("OTLP endpoint = %q; want the collector service on 4318", cfg.Telemetry.OTLPEndpoint)
	}

	// Logs go over OTLP like traces and metrics, with the same service name
	// and endpoint; there is no Docker log driver in between.
	if cfg.Telemetry.LogsExporter != config.TelemetryLogsExporterOTLP {
		t.Errorf("logs exporter = %q; want otlp so application logs reach Loki through the collector", cfg.Telemetry.LogsExporter)
	}
	if level, ok := cfg.Telemetry.LogLevel(); !ok || level != slog.LevelInfo {
		t.Errorf("logs level = %q; want info", cfg.Telemetry.LogsLevel)
	}
	if _, ok := app["logging"]; ok {
		t.Errorf("app logging = %v; the app's logs travel over OTLP, not a log driver", app["logging"])
	}
	overlay, err := os.ReadFile(filepath.Join(dir, "compose.observability.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"fluentd", "fluent_forward", "24224"} {
		if strings.Contains(string(overlay), gone) {
			t.Errorf("the overlay still mentions %q", gone)
		}
	}
}

func TestObservabilityConfigurationAgreesAcrossFiles(t *testing.T) {
	dir := generateObservability(t, "postgresql")
	services := mapAt(t, readYAML(t, dir, "compose.observability.yaml"), "services")

	// Collector: exporters point at services that exist, on ports they serve.
	collector := readYAML(t, dir, "observability/otel-collector.yaml")
	exporters := mapAt(t, collector, "exporters")
	for exporter, wantHost := range map[string]string{
		"otlp_http/tempo":      "tempo:4318",
		"otlp_http/prometheus": "prometheus:9090",
		"otlp_http/loki":       "loki:3100",
	} {
		endpoint, _ := mapAt(t, exporters, exporter)["endpoint"].(string)
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host != wantHost {
			t.Errorf("collector exporter %s endpoint = %q, want host %s", exporter, endpoint, wantHost)
		}
		if _, ok := services[strings.Split(wantHost, ":")[0]]; !ok {
			t.Errorf("collector exporter %s targets a service the overlay does not define", exporter)
		}
	}
	pipelines := mapAt(t, collector, "service", "pipelines")
	for pipeline, receiver := range map[string]string{"traces": "otlp", "metrics": "otlp", "logs": "otlp"} {
		got := stringList(t, mapAt(t, pipelines, pipeline)["receivers"])
		if len(got) != 1 || got[0] != receiver {
			t.Errorf("%s pipeline receivers = %v, want [%s]", pipeline, got, receiver)
		}
	}
	collectorService := mapAt(t, services, "otel-collector")
	if ports := strings.Join(stringList(t, collectorService["ports"]), " "); !strings.Contains(ports, ":4318") || strings.Contains(ports, ":24224") {
		t.Errorf("collector ports = %s; want only OTLP 4318 (logs arrive over OTLP, there is no fluent intake)", ports)
	}
	if _, ok := mapAt(t, collector, "receivers")["fluent_forward"]; ok {
		t.Error("the collector must not keep a fluent_forward receiver")
	}
	if _, ok := mapAt(t, collector, "processors")["transform/logs"]; ok {
		t.Error("log records arrive structured; the collector needs no log parsing")
	}

	// Tempo and Loki serve what the collector and Grafana expect.
	tempo := readYAML(t, dir, "observability/tempo.yaml")
	if got := mapAt(t, tempo, "distributor", "receivers", "otlp", "protocols", "http")["endpoint"]; got != "0.0.0.0:4318" {
		t.Errorf("tempo OTLP/HTTP endpoint = %v", got)
	}
	if got := mapAt(t, tempo, "server")["http_listen_port"]; got != 3200 {
		t.Errorf("tempo HTTP port = %v, want 3200", got)
	}
	if got := mapAt(t, readYAML(t, dir, "observability/loki.yaml"), "server")["http_listen_port"]; got != 3100 {
		t.Errorf("loki HTTP port = %v, want 3100", got)
	}
	command := strings.Join(stringList(t, mapAt(t, services, "prometheus")["command"]), " ")
	if !strings.Contains(command, "--web.enable-otlp-receiver") {
		t.Errorf("prometheus command %q must enable the OTLP receiver the collector writes to", command)
	}

	// Grafana data sources: reachable service names, correlation configured.
	datasources := readYAML(t, dir, "observability/grafana/provisioning/datasources/datasources.yaml")
	byUID := map[string]map[string]any{}
	items, _ := datasources["datasources"].([]any)
	for _, item := range items {
		source := item.(map[string]any)
		byUID[source["uid"].(string)] = source
	}
	for uid, wantURL := range map[string]string{"prometheus": "http://prometheus:9090", "tempo": "http://tempo:3200", "loki": "http://loki:3100"} {
		source, ok := byUID[uid]
		if !ok {
			t.Fatalf("datasource %q is not provisioned", uid)
		}
		if source["url"] != wantURL {
			t.Errorf("datasource %s url = %v, want %s", uid, source["url"], wantURL)
		}
	}
	toLogs := mapAt(t, byUID["tempo"], "jsonData", "tracesToLogsV2")
	if toLogs["datasourceUid"] != "loki" {
		t.Errorf("tempo trace-to-logs targets %v, want loki", toLogs["datasourceUid"])
	}
	// trace_id is structured metadata of the OTLP log record, not text of the
	// line, so the link must filter on it and not use a line filter.
	if query, _ := toLogs["query"].(string); toLogs["customQuery"] != true || !strings.Contains(query, `| trace_id="$${__span.traceId}"`) || !strings.Contains(query, "__tags") {
		t.Errorf("tempo trace-to-logs = %v; want a custom query filtering on the trace_id structured metadata", toLogs)
	}
	derived, _ := mapAt(t, byUID["loki"], "jsonData")["derivedFields"].([]any)
	if len(derived) != 1 {
		t.Fatalf("loki derived fields = %v, want one trace_id field", derived)
	}
	field := derived[0].(map[string]any)
	if field["datasourceUid"] != "tempo" {
		t.Errorf("loki trace_id field links to %v, want tempo", field["datasourceUid"])
	}
	if field["matcherType"] != "label" || field["matcherRegex"] != "trace_id" {
		t.Errorf("loki trace_id field = %v; want it to read the trace_id label or structured metadata", field)
	}
}

func TestObservabilityDashboard(t *testing.T) {
	dir := generateObservability(t, "postgresql")
	content, err := os.ReadFile(filepath.Join(dir, "observability", "grafana", "dashboards", "forge-application.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		UID    string `json:"uid"`
		Title  string `json:"title"`
		Panels []struct {
			ID         int    `json:"id"`
			Type       string `json:"type"`
			Title      string `json:"title"`
			Datasource struct {
				UID string `json:"uid"`
			} `json:"datasource"`
			Targets []struct {
				Expr  string `json:"expr"`
				Query string `json:"query"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(content, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.UID == "" || dashboard.Title == "" {
		t.Errorf("dashboard needs a uid and a title: %+v", dashboard)
	}
	ids := map[int]bool{}
	wanted := map[string]bool{
		"http_server_request_duration_seconds_bucket":   false,
		"http_server_request_duration_seconds_count":    false,
		"db_client_operation_duration_seconds_bucket":   false,
		"forge_queue_job_duration_seconds_count":        false,
		"forge_telemetry_spans_dropped_total":           false,
		"forge_telemetry_logs_dropped_total":            false,
		"db_client_connection_count":                    false,
		"db_client_connection_max":                      false,
		"forge_db_pool_waits_total":                     false,
		"forge_db_pool_wait_duration_seconds_total":     false,
		"go_goroutine_count":                            false,
		"go_memory_heap_alloc_bytes":                    false,
		"go_memory_heap_inuse_bytes":                    false,
		"go_memory_allocated_bytes_total":               false,
		"go_gc_cycles_total":                            false,
		"go_gc_pause_duration_seconds_bucket":           false,
		`http_response_status_class="5xx"`:              false,
		`{service_name=~"$service"}`:                    false,
		`{ resource.service.name =~ "$service" }`:       false,
		"count_over_time({service_name=~\"$service\"}[": false,
	}
	for _, panel := range dashboard.Panels {
		if ids[panel.ID] {
			t.Errorf("duplicate panel id %d", panel.ID)
		}
		ids[panel.ID] = true
		if panel.Type == "row" {
			continue
		}
		if panel.Datasource.UID != "prometheus" && panel.Datasource.UID != "loki" && panel.Datasource.UID != "tempo" {
			t.Errorf("panel %q uses unknown datasource %q", panel.Title, panel.Datasource.UID)
		}
		if len(panel.Targets) == 0 {
			t.Errorf("panel %q has no queries", panel.Title)
		}
		for _, target := range panel.Targets {
			for needle := range wanted {
				if strings.Contains(target.Expr+target.Query, needle) {
					wanted[needle] = true
				}
			}
		}
	}
	for needle, seen := range wanted {
		if !seen {
			t.Errorf("no dashboard panel queries %s", needle)
		}
	}
}

func TestObservabilityMetricNamesExistInTheDocumentedCatalogue(t *testing.T) {
	// The dashboard may only query metrics the framework exports; the names
	// are listed in docs/OBSERVABILITY.md.
	catalogue, err := os.ReadFile(filepath.Join("..", "..", "docs", "OBSERVABILITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	dir := generateObservability(t, "postgresql")
	content, err := os.ReadFile(filepath.Join(dir, "observability", "grafana", "dashboards", "forge-application.json"))
	if err != nil {
		t.Fatal(err)
	}
	metric := regexp.MustCompile(`\b((?:http|db|forge|go)_[a-z_]+?)(?:_bucket|_count|_sum)?\b[{\[(]`)
	for _, match := range metric.FindAllStringSubmatch(string(content), -1) {
		name := match[1]
		if !strings.Contains(string(catalogue), name) {
			t.Errorf("dashboard queries %q, which docs/OBSERVABILITY.md does not list", name)
		}
	}
}

// TestObservabilityAppNamesThatAreYAMLKeywordsStayStrings guards the
// overlay against application names such as null, true or false, which the
// name pattern allows and YAML would otherwise read as null or booleans.
func TestObservabilityAppNamesThatAreYAMLKeywordsStayStrings(t *testing.T) {
	for _, name := range []string{"null", "true", "false", "yes", "off"} {
		dir := filepath.Join(t.TempDir(), name)
		if _, err := scaffold.Generate(scaffold.Options{Name: name, Dir: dir, Database: "sqlite"}); err != nil {
			t.Fatalf("generate %q: %v", name, err)
		}
		app := mapAt(t, readYAML(t, dir, "compose.observability.yaml"), "services", "app")
		if got := mapAt(t, app, "environment")["FORGE_TELEMETRY_SERVICE_NAME"]; got != name {
			t.Errorf("app %q: FORGE_TELEMETRY_SERVICE_NAME = %#v; want the string %q", name, got, name)
		}
	}
}
