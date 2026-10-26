package forge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/openapi"
)

type greeting struct {
	Message string `json:"message"`
}

func describedRoutes(_ context.Context, app *forge.App) error {
	if err := app.SetAPIInfo(openapi.Info{Title: "greeter", Version: "1.2.3"}); err != nil {
		return err
	}
	hello := openapi.Typed(http.StatusOK, openapi.Operation{Summary: "Say hello", Tags: []string{"greetings"}},
		func(*http.Request, openapi.NoBody) (greeting, error) { return greeting{Message: "hello"}, nil })
	if err := app.Handle("GET /v1/hello", hello); err != nil {
		return err
	}
	return app.HandleFunc("GET /plain", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func newDescribedApp(t *testing.T, cfg config.Config) *forge.App {
	t.Helper()
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := describedRoutes(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestAppServesItsOpenAPIDocumentInDevelopment(t *testing.T) {
	t.Parallel()
	app := newDescribedApp(t, config.Default())
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, forge.OpenAPIPath, nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET %s: status=%d headers=%v", forge.OpenAPIPath, recorder.Code, recorder.Header())
	}
	var document struct {
		OpenAPI string                    `json:"openapi"`
		Info    map[string]string         `json:"info"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("document is not JSON: %v", err)
	}
	if document.OpenAPI != "3.1.0" || document.Info["title"] != "greeter" || document.Info["version"] != "1.2.3" {
		t.Fatalf("header = %s %v", document.OpenAPI, document.Info)
	}
	for _, path := range []string{"/v1/hello", "/health", "/health/live", "/health/ready"} {
		if _, ok := document.Paths[path]["get"]; !ok {
			t.Errorf("%s is not documented: %v", path, document.Paths)
		}
	}
	for _, path := range []string{"/plain", forge.OpenAPIPath} {
		if _, ok := document.Paths[path]; ok {
			t.Errorf("%s is documented", path)
		}
	}

	// The built-in routes answer what they declare.
	built, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if got := built.Undocumented(); !slices.Equal(got, []string{"GET /plain"}) {
		t.Fatalf("Undocumented = %v", got)
	}
	if !bytes.Equal(built.JSON(), recorder.Body.Bytes()) {
		t.Fatal("the served document differs from App.OpenAPI")
	}
	for _, path := range []string{"/v1/hello", "/health", "/health/live", "/health/ready"} {
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if err := built.ValidateResponse(http.MethodGet, path, response.Code, response.Header(), response.Body.Bytes()); err != nil {
			t.Errorf("GET %s: %v", path, err)
		}
	}
}

func TestOpenAPIDocumentIsNotServedInProductionUnlessEnabled(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Environment = config.Production
	cfg.HTTP.Transport = config.TransportTrustedProxy
	app := newDescribedApp(t, cfg)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, forge.OpenAPIPath, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("production served the document: status=%d", recorder.Code)
	}
	// The document can still be built, for `forge openapi`.
	if _, err := app.OpenAPI(); err != nil {
		t.Fatal(err)
	}

	enabled := true
	cfg.OpenAPI.Enabled = &enabled
	app = newDescribedApp(t, cfg)
	recorder = httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, forge.OpenAPIPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("FORGE_OPENAPI_ENABLED=true: status=%d", recorder.Code)
	}
}

func TestSetAPIInfoNeedsTitleAndVersion(t *testing.T) {
	t.Parallel()
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetAPIInfo(openapi.Info{Title: "only a title"}); err == nil {
		t.Fatal("SetAPIInfo accepted an empty version")
	}
}

func TestExecuteOpenAPIWritesAndChecksTheDocument(t *testing.T) {
	t.Parallel()
	lookup := envLookup(map[string]string{"FORGE_ENV": "test"})
	var stdout, stderr bytes.Buffer
	if code := forge.Execute(context.Background(), []string{"openapi"}, lookup, &stdout, &stderr, describedRoutes); code != 0 {
		t.Fatalf("openapi: code=%d stderr=%s", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"/v1/hello"`) {
		t.Fatalf("stdout is not the document:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "1 routes are not described") || !strings.Contains(stderr.String(), "GET /plain") {
		t.Fatalf("stderr does not list the undocumented route: %s", stderr.String())
	}

	file := filepath.Join(t.TempDir(), "openapi.json")
	stderr.Reset()
	if code := forge.Execute(context.Background(), []string{"openapi", "--output", file}, lookup, &bytes.Buffer{}, &stderr, describedRoutes); code != 0 {
		t.Fatalf("openapi --output: code=%d stderr=%s", code, stderr.String())
	}
	written, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(written, stdout.Bytes()) {
		t.Fatalf("--output wrote %q (err %v)", written, err)
	}
	if code := forge.Execute(context.Background(), []string{"openapi", "--check", file}, lookup, &bytes.Buffer{}, &stderr, describedRoutes); code != 0 {
		t.Fatalf("openapi --check of a fresh file: code=%d stderr=%s", code, stderr.String())
	}
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := forge.Execute(context.Background(), []string{"openapi", "--check", file}, lookup, &bytes.Buffer{}, &stderr, describedRoutes); code != 1 || !strings.Contains(stderr.String(), "out of date") {
		t.Fatalf("openapi --check of a stale file: code=%d stderr=%s", code, stderr.String())
	}
	for _, args := range [][]string{{"openapi", "extra"}, {"openapi", "--output", "a", "--check", "b"}, {"openapi", "--unknown"}} {
		if code := forge.Execute(context.Background(), args, lookup, &bytes.Buffer{}, &bytes.Buffer{}, describedRoutes); code != 2 {
			t.Errorf("%v: code=%d, want 2", args, code)
		}
	}
}

func TestInaccurateDescriptionStopsStartup(t *testing.T) {
	t.Parallel()
	configure := func(_ context.Context, app *forge.App) error {
		// A path parameter the route does not have.
		return app.Handle("GET /v1/things", openapi.Describe(http.NotFoundHandler(), openapi.Operation{
			Params: openapi.TypeOf[struct {
				ID string `path:"id"`
			}](),
			Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)},
		}))
	}
	lookup := envLookup(map[string]string{"FORGE_ENV": "test", "FORGE_HTTP_ADDR": anyAddress})
	var stdout, stderr bytes.Buffer
	if code := forge.Execute(context.Background(), nil, lookup, &stdout, &stderr, configure); code != 1 || !strings.Contains(stdout.String()+stderr.String(), "not a wildcard") {
		t.Fatalf("serve: code=%d output=%s%s", code, stdout.String(), stderr.String())
	}
	if code := forge.Execute(context.Background(), []string{"openapi"}, lookup, &stdout, &stderr, configure); code != 1 {
		t.Fatalf("openapi: code=%d", code)
	}
}

// An application that already served a hand-written document keeps it.
func TestApplicationRouteAtOpenAPIPathWins(t *testing.T) {
	t.Parallel()
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.HandleFunc("GET "+forge.OpenAPIPath, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hand":"written"}`))
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, forge.OpenAPIPath, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"hand":"written"}` {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if got := document.Undocumented(); !slices.Equal(got, []string{"GET " + forge.OpenAPIPath}) {
		t.Fatalf("Undocumented = %v", got)
	}
}

// A route registered after Run starts but before the first request (the
// router freezes then) is in the served document.
func TestServedDocumentIncludesRoutesAddedBeforeTheFirstRequest(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Environment = config.Test
	cfg.HTTP.Address = anyAddress
	var logs safeBuffer
	app, err := forge.New(forge.WithConfig(cfg), forge.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	// Wait for the listener without sending a request.
	address := boundAddress(t, &logs, "serving HTTP")
	select {
	case err := <-done:
		t.Fatalf("the application stopped before the request: %v", err)
	default:
	}
	late := openapi.Typed(http.StatusOK, openapi.Operation{}, func(*http.Request, openapi.NoBody) (greeting, error) { return greeting{}, nil })
	if err := app.Handle("GET /v1/late", late); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get("http://" + address + forge.OpenAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var document struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	if _, ok := document.Paths["/v1/late"]; !ok {
		t.Fatalf("/v1/late is missing from the served document: %v", document.Paths)
	}
}
