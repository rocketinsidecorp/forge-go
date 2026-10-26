package forge_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
)

const (
	routesPath = "/v1/widgets"
	routesRead = "GET "
)

type widgets struct{}

func (widgets) Index(http.ResponseWriter, *http.Request) {}
func (widgets) Show(http.ResponseWriter, *http.Request)  {}

// Routes built from constants and a loop, as the generated JSON API does, are
// invisible to a source scan but are registered when the application composes.
func TestExecuteRoutesListsRoutesBuiltFromConstants(t *testing.T) {
	t.Parallel()
	configure := func(_ context.Context, app *forge.App) error {
		for _, pattern := range []string{routesRead + routesPath, "POST " + routesPath, routesRead + routesPath + "/{id}"} {
			if err := app.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {}); err != nil {
				return err
			}
		}
		return app.Resources("/gadgets", widgets{})
	}
	var stdout, stderr bytes.Buffer
	lookup := envLookup(map[string]string{"FORGE_ENV": "development"})
	if code := forge.Execute(context.Background(), []string{"routes"}, lookup, &stdout, &stderr, configure); code != 0 {
		t.Fatalf("routes: code=%d stderr=%s", code, stderr.String())
	}
	var table []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line == "" {
			break
		}
		table = append(table, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"METHOD PATH ACTION SOURCE",
		"GET /gadgets index forge_routes_test.go:",
		"GET /gadgets/{id} show forge_routes_test.go:",
		"GET /health - forge (built in)",
		"GET /health/live - forge (built in)",
		"GET /health/ready - forge (built in)",
		"GET /openapi.json - forge (built in; development and test, or FORGE_OPENAPI_ENABLED=true)",
		"GET /v1/widgets - forge_routes_test.go:",
		"POST /v1/widgets - forge_routes_test.go:",
		"GET /v1/widgets/{id} - forge_routes_test.go:",
	}
	if len(table) != len(want) {
		t.Fatalf("table:\n%s", strings.Join(table, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(table[i], want[i]) {
			t.Errorf("row %d = %q, want prefix %q", i, table[i], want[i])
		}
	}
	if !strings.Contains(stdout.String(), "Scope: every route registered while composing the application") {
		t.Errorf("no scope footer:\n%s", stdout.String())
	}
}

func TestExecuteRoutesOmitsTheOpenAPIRouteWhereItIsNotServed(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	lookup := envLookup(map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_HTTP_TRUSTED_PROXIES": "10.0.0.0/8"})
	configure := func(context.Context, *forge.App) error { return nil }
	if code := forge.Execute(context.Background(), []string{"routes"}, lookup, &stdout, &stderr, configure); code != 0 {
		t.Fatalf("routes: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "openapi.json") || !strings.Contains(stdout.String(), "/health/ready") {
		t.Fatalf("table:\n%s", stdout.String())
	}
}

func TestExecuteRoutesReportsFailures(t *testing.T) {
	t.Parallel()
	lookup := envLookup(map[string]string{"FORGE_ENV": "test"})
	var stderr bytes.Buffer
	failing := func(context.Context, *forge.App) error { return errors.New("no database") }
	if code := forge.Execute(context.Background(), []string{"routes"}, lookup, &bytes.Buffer{}, &stderr, failing); code != 1 || !strings.Contains(stderr.String(), "routes: configure application: no database") {
		t.Errorf("configure failure: code=%d stderr=%s", code, stderr.String())
	}
	ok := func(context.Context, *forge.App) error { return nil }
	if code := forge.Execute(context.Background(), []string{"routes"}, lookup, failingWriter{}, &stderr, ok); code != 1 {
		t.Errorf("a failing output: code=%d", code)
	}
	if code := forge.Execute(context.Background(), []string{"routes", "extra"}, lookup, &bytes.Buffer{}, &stderr, ok); code != 2 {
		t.Errorf("an argument: code=%d, want 2", code)
	}
	command := forge.Command{Name: "routes", Summary: "x", Run: func(context.Context, forge.CommandEnv, []string) error { return nil }}
	if code := forge.Execute(context.Background(), []string{"help"}, lookup, &bytes.Buffer{}, &stderr, ok, command); code != 2 {
		t.Errorf("an extra command named routes: code=%d, want 2", code)
	}
}
