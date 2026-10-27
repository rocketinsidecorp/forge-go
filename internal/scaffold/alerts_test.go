package scaffold_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// exporter is what the framework registers: Prometheus metric name to kind
// and label names (OpenTelemetry attribute names with dots turned into
// underscores, as Prometheus receives them).
type exporter map[string]struct {
	kind   telemetry.MetricKind
	labels []string
}

// frameworkExporter reads the framework's non-test Go files and collects every
// metric registered with Registry.Counter, Gauge or Histogram (and the
// runtime package's gauge helper): the name, resolved from a literal or a
// string constant, the unit and the label names. The Prometheus name comes
// from telemetry.PrometheusName, the function the exporter itself uses.
func frameworkExporter(t *testing.T) exporter {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	packages := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "examples", "testdata", "internal":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		packages[filepath.Dir(file)] = append(packages[filepath.Dir(file)], parsed)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// String constants, per package directory and, for selectors such as
	// telemetry.MetricGoroutines, by name across the framework.
	constants := map[string]map[string]string{}
	byName := map[string]string{}
	for dir, files := range packages {
		constants[dir] = map[string]string{}
		for _, parsed := range files {
			ast.Inspect(parsed, func(node ast.Node) bool {
				spec, ok := node.(*ast.ValueSpec)
				if !ok || len(spec.Names) != len(spec.Values) {
					return true
				}
				for i, value := range spec.Values {
					if literal, ok := value.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						text, _ := strconv.Unquote(literal.Value)
						constants[dir][spec.Names[i].Name] = text
						byName[spec.Names[i].Name] = text
					}
				}
				return true
			})
		}
	}

	found := exporter{}
	for dir, files := range packages {
		resolve := func(expression ast.Expr) (string, bool) {
			switch value := expression.(type) {
			case *ast.BasicLit:
				text, err := strconv.Unquote(value.Value)
				return text, err == nil && value.Kind == token.STRING
			case *ast.Ident:
				text, ok := constants[dir][value.Name]
				return text, ok
			case *ast.SelectorExpr:
				text, ok := byName[value.Sel.Name]
				return text, ok
			}
			return "", false
		}
		for _, parsed := range files {
			ast.Inspect(parsed, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) < 3 {
					return true
				}
				var kind telemetry.MetricKind
				labelsFrom := 3
				switch callee := call.Fun.(type) {
				case *ast.SelectorExpr:
					switch callee.Sel.Name {
					case "Counter", "FloatCounter":
						kind = telemetry.MetricCounter
					case "Gauge":
						kind = telemetry.MetricGauge
					case "Histogram":
						kind, labelsFrom = telemetry.MetricHistogram, 4
					default:
						return true
					}
				case *ast.Ident:
					if callee.Name != "gauge" {
						return true
					}
					kind = telemetry.MetricGauge
				default:
					return true
				}
				name, okName := resolve(call.Args[0])
				unit, okUnit := resolve(call.Args[2])
				if !okName || !okUnit {
					return true
				}
				var labels []string
				if len(call.Args) > labelsFrom {
					for _, argument := range call.Args[labelsFrom:] {
						if label, ok := resolve(argument); ok {
							labels = append(labels, strings.NewReplacer(".", "_").Replace(label))
						}
					}
				}
				found[telemetry.PrometheusName(name, unit, kind)] = struct {
					kind   telemetry.MetricKind
					labels []string
				}{kind, labels}
				return true
			})
		}
	}
	return found
}

var (
	promString    = regexp.MustCompile(`"[^"]*"`)
	promMatchers  = regexp.MustCompile(`([A-Za-z_:][A-Za-z0-9_:]*)\s*\{([^}]*)\}`)
	promGrouping  = regexp.MustCompile(`\b(?:by|without)\s*\(([^)]*)\)`)
	promLabelName = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:=~|!=|!~|=)`)
	promToken     = regexp.MustCompile(`\b[A-Za-z_:][A-Za-z0-9_:]*\b`)
)

// promqlWords are the PromQL functions, aggregations and operators the rules
// use; every other word of an expression must be an exported metric.
var promqlWords = []string{
	"sum", "rate", "increase", "histogram_quantile", "absent_over_time", "and", "or", "unless", "on", "ignoring",
}

// builtinMetrics are series Prometheus itself creates for every target.
var builtinMetrics = []string{"up"}

// labelsEveryMetricHas are labels Prometheus and the histogram buckets add.
var labelsEveryMetricHas = []string{"job", "instance", "le"}

// exprProblems returns what is wrong with a PromQL expression: a metric that
// the framework does not export, or a label the metrics it uses do not carry,
// and the exported metrics it uses.
func exprProblems(expr string, known exporter) (problems, used []string) {
	allowedLabels := slices.Clone(labelsEveryMetricHas)
	metricOf := func(token string) (string, bool) {
		if slices.Contains(builtinMetrics, token) {
			return token, true
		}
		if _, ok := known[token]; ok {
			return token, true
		}
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if base, ok := strings.CutSuffix(token, suffix); ok && known[base].kind == telemetry.MetricHistogram {
				return base, true
			}
		}
		return "", false
	}

	text := promString.ReplaceAllString(expr, `""`)
	var labels []string
	text = promMatchers.ReplaceAllStringFunc(text, func(match string) string {
		parts := promMatchers.FindStringSubmatch(match)
		for _, label := range promLabelName.FindAllStringSubmatch(parts[2], -1) {
			labels = append(labels, label[1])
		}
		return parts[1]
	})
	text = promGrouping.ReplaceAllStringFunc(text, func(match string) string {
		labels = append(labels, promToken.FindAllString(promGrouping.FindStringSubmatch(match)[1], -1)...)
		return ""
	})
	for _, token := range promToken.FindAllString(text, -1) {
		if slices.Contains(promqlWords, token) {
			continue
		}
		metric, ok := metricOf(token)
		if !ok {
			problems = append(problems, fmt.Sprintf("%q is not a metric the framework exports", token))
			continue
		}
		used = append(used, metric)
		allowedLabels = append(allowedLabels, known[metric].labels...)
	}
	for _, label := range labels {
		if !slices.Contains(allowedLabels, label) {
			problems = append(problems, fmt.Sprintf("label %q is not carried by %s", label, strings.Join(used, ", ")))
		}
	}
	return problems, used
}

// alertRules reads observability/alerts.yml of a generated application.
func alertRules(t *testing.T) (rules []map[string]any, dir string) {
	t.Helper()
	dir = generateObservability(t, "sqlite")
	document := readYAML(t, dir, "observability/alerts.yml")
	groups, _ := document["groups"].([]any)
	if len(groups) == 0 {
		t.Fatal("alerts.yml has no groups")
	}
	for _, group := range groups {
		items, _ := group.(map[string]any)["rules"].([]any)
		for _, item := range items {
			rules = append(rules, item.(map[string]any))
		}
	}
	return rules, dir
}

func TestAlertRulesAreWellFormed(t *testing.T) {
	rules, dir := alertRules(t)
	seen := map[string]bool{}
	for _, rule := range rules {
		name, _ := rule["alert"].(string)
		if name == "" || seen[name] {
			t.Errorf("alert name %q is empty or repeated", name)
		}
		seen[name] = true
		if expr, _ := rule["expr"].(string); strings.TrimSpace(expr) == "" {
			t.Errorf("%s has no expr", name)
		}
		labels := mapAt(t, rule, "labels")
		if severity := labels["severity"]; severity != "warning" && severity != "critical" {
			t.Errorf("%s severity = %v; want warning or critical", name, severity)
		}
		annotations := mapAt(t, rule, "annotations")
		for _, key := range []string{"summary", "description"} {
			if text, _ := annotations[key].(string); text == "" {
				t.Errorf("%s has no %s annotation", name, key)
			}
		}
	}
	// What the task of an SLO set must cover.
	for _, want := range []string{"ForgeTargetDown", "ForgeApplicationMetricsMissing", "ForgeHighServerErrorRatio", "ForgeHighRequestLatencyP95", "ForgeDatabasePoolSaturated", "ForgeQueueJobsDead"} {
		if !seen[want] {
			t.Errorf("alerts.yml has no %s alert", want)
		}
	}

	// Prometheus loads the file the overlay mounts.
	files := stringList(t, readYAML(t, dir, "observability/prometheus.yml")["rule_files"])
	if len(files) != 1 || files[0] != "/etc/prometheus/alerts.yml" {
		t.Fatalf("prometheus.yml rule_files = %v; want [/etc/prometheus/alerts.yml]", files)
	}
	volumes := stringList(t, mapAt(t, readYAML(t, dir, "compose.observability.yaml"), "services", "prometheus")["volumes"])
	if !slices.Contains(volumes, "./observability/alerts.yml:"+files[0]+":ro") {
		t.Errorf("the prometheus service mounts %v; want ./observability/alerts.yml at %s", volumes, files[0])
	}
}

func TestAlertRulesUseOnlyMetricsTheFrameworkExports(t *testing.T) {
	known := frameworkExporter(t)
	// The scan must find the catalogue, or it checks nothing.
	for _, name := range []string{
		"http_server_request_duration_seconds", "db_client_connection_count", "db_client_connection_max",
		"forge_db_pool_wait_duration_seconds_total", "forge_queue_job_duration_seconds",
		"forge_scheduler_job_duration_seconds", "go_goroutine_count",
	} {
		if _, ok := known[name]; !ok {
			t.Fatalf("the scan of the framework's metric registrations did not find %s (found %d metrics)", name, len(known))
		}
	}
	// Every exported metric a rule uses is in the catalogue users read.
	catalogue, err := os.ReadFile(filepath.Join("..", "..", "docs", "OBSERVABILITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := alertRules(t)
	for _, rule := range rules {
		problems, used := exprProblems(rule["expr"].(string), known)
		for _, problem := range problems {
			t.Errorf("alert %v: %s", rule["alert"], problem)
		}
		for _, metric := range used {
			if metric != "up" && !strings.Contains(string(catalogue), "`"+metric+"`") {
				t.Errorf("alert %v uses %s, which docs/OBSERVABILITY.md does not list", rule["alert"], metric)
			}
		}
	}
}

// TestAlertRuleCheckRejectsWhatItShouldReject keeps the check above honest.
func TestAlertRuleCheckRejectsWhatItShouldReject(t *testing.T) {
	known := frameworkExporter(t)
	for expr, want := range map[string]string{
		`sum(rate(http_server_request_duration_seconds_count[5m]))`:                            "",
		`sum by (le) (rate(http_server_request_duration_seconds_bucket{http_route="/x"}[5m]))`: "",
		`up == 0`: "",
		`db_client_connection_count{db_client_connection_state="used"}`:                              "",
		`http_server_requests_total`:                                                                 `"http_server_requests_total" is not a metric`,
		`rate(http_server_request_duration_seconds_total[5m])`:                                       `"http_server_request_duration_seconds_total" is not a metric`,
		`db_client_connection_count_bucket`:                                                          `"db_client_connection_count_bucket" is not a metric`,
		`sum(rate(http_server_request_duration_seconds_count{status="5xx"}[5m]))`:                    `label "status" is not carried`,
		`sum by (route) (rate(http_server_request_duration_seconds_count[5m]))`:                      `label "route" is not carried`,
		`rate(forge_queue_job_duration_seconds_count{forge_job_outcome="dead"}[5m]) > 0 and mystery`: `"mystery" is not a metric`,
	} {
		found, _ := exprProblems(expr, known)
		problems := strings.Join(found, "; ")
		if want == "" && problems != "" || want != "" && !strings.Contains(problems, want) {
			t.Errorf("%s: problems = %q, want %q", expr, problems, want)
		}
	}
}
