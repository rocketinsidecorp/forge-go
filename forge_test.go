package forge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
)

func TestAppHealthAndApplicationRoute(t *testing.T) {
	t.Parallel()
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }); err != nil {
		t.Fatal(err)
	}
	if err := app.Readiness("dependency", func(context.Context) error { return errors.New("unavailable") }); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path       string
		wantStatus int
	}{
		{path: "/health/live", wantStatus: http.StatusOK},
		{path: "/health/ready", wantStatus: http.StatusServiceUnavailable},
		{path: "/hello", wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}
