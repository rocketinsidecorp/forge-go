package bootstrap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/bootstrap"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/db"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

func TestConfigureRequiresDatabase(t *testing.T) {
	t.Parallel()
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Configure(context.Background(), app); err == nil {
		t.Fatal("Configure succeeded without FORGE_DATABASE_URL")
	}
}

// TestServiceEndToEnd drives the binary's real entry points: the migrate
// command main registers, then Configure against the migrated database.
func TestServiceEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database = postgrestest.Config(t)

	env := map[string]string{"FORGE_DATABASE_URL": cfg.Database.URL.Reveal()}
	lookup := func(key string) (string, bool) { value, ok := env[key]; return value, ok }
	var stdout, stderr bytes.Buffer
	code := forge.Execute(ctx, []string{"migrate"}, lookup, &stdout, &stderr, bootstrap.Configure, postgres.Commands(db.Migrations())...)
	if code != 0 || !strings.Contains(stdout.String(), "create_tasks") {
		t.Fatalf("migrate exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Configure(ctx, app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	handler := app.Handler()
	serve := func(request *http.Request) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	create := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"title":"wired"}`))
	create.Header.Set("Content-Type", "application/json")
	recorder := serve(create)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", recorder.Code, recorder.Body)
	}
	recorder = serve(httptest.NewRequest(http.MethodGet, recorder.Header().Get("Location"), nil))
	var fetched struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &fetched); recorder.Code != http.StatusOK || err != nil || fetched.Title != "wired" {
		t.Fatalf("get: status %d, body %s", recorder.Code, recorder.Body)
	}

	if recorder = serve(httptest.NewRequest(http.MethodGet, "/health/ready", nil)); recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), `{"name":"database","healthy":true}`) {
		t.Fatalf("ready: status %d, body %s", recorder.Code, recorder.Body)
	}

	// The shutdown hook closes the pool, which readiness then reports.
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if recorder = serve(httptest.NewRequest(http.MethodGet, "/health/ready", nil)); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready after close: status %d, body %s", recorder.Code, recorder.Body)
	}
}
