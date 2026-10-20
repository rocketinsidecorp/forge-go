package httpserver_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/httpserver"
	"github.com/rocketinsidecorp/forge-go/web"
)

func TestServerMiddleware(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		handler    http.Handler
		body       string
		wantStatus int
	}{
		{name: "success", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), wantStatus: http.StatusNoContent},
		{name: "oversized body", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), body: "12345", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "panic", handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("sensitive panic") }), wantStatus: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.HTTP.MaxBodyBytes = 4
			server, err := httpserver.New(cfg, test.handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d", recorder.Code, test.wantStatus)
			}
			// X-Frame-Options and CSP are checked because http.Error sets nosniff on its own.
			if recorder.Header().Get("X-Request-ID") == "" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" ||
				recorder.Header().Get("X-Frame-Options") != "DENY" || recorder.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing secure response headers")
			}
			if strings.Contains(recorder.Body.String(), "sensitive panic") {
				t.Fatal("panic leaked to client")
			}
		})
	}
}

func TestRequestContextCarriesRequestID(t *testing.T) {
	t.Parallel()
	var seen string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = web.RequestID(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	server, err := httpserver.New(config.Default(), handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if seen == "" || seen != recorder.Header().Get("X-Request-ID") {
		t.Fatalf("context request ID %q does not match header %q", seen, recorder.Header().Get("X-Request-ID"))
	}
}

func TestNewWithMiddlewarePreservesFrameworkContextAndOrder(t *testing.T) {
	t.Parallel()
	calls := []string{}
	middleware := func(name string) httpserver.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if web.RequestID(r.Context()) == "" {
					t.Error("request ID was not set before custom middleware")
				}
				calls = append(calls, name+" before")
				next.ServeHTTP(w, r)
				calls = append(calls, name+" after")
			})
		}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls = append(calls, "handler")
		w.WriteHeader(http.StatusNoContent)
	})
	server, err := httpserver.NewWithMiddleware(
		config.Default(),
		handler,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		middleware("first"),
		middleware("second"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	want := []string{"first before", "second before", "handler", "second after", "first after"}
	if !slices.Equal(calls, want) {
		t.Fatalf("middleware calls=%v want=%v", calls, want)
	}
}

func TestNewWithMiddlewareRejectsInvalidMiddleware(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if _, err := httpserver.NewWithMiddleware(config.Default(), handler, logger, nil); err == nil {
		t.Fatal("nil middleware was accepted")
	}
	nilHandlerMiddleware := httpserver.Middleware(func(http.Handler) http.Handler { return nil })
	if _, err := httpserver.NewWithMiddleware(config.Default(), handler, logger, nilHandlerMiddleware); err == nil {
		t.Fatal("middleware returning nil handler was accepted")
	}
}

func TestNewWithMiddlewarePanicUsesFrameworkRecovery(t *testing.T) {
	t.Parallel()
	middleware := httpserver.Middleware(func(http.Handler) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("middleware failure")
		})
	})
	server, err := httpserver.NewWithMiddleware(
		config.Default(),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		middleware,
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusInternalServerError)
	}
	if recorder.Header().Get("X-Request-ID") == "" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("framework headers were not preserved after middleware panic")
	}
	if strings.Contains(recorder.Body.String(), "middleware failure") {
		t.Fatal("middleware panic leaked to client")
	}
}

func TestInstrumentWrapsRecoveryAndBodyLimitInsideTheRequestID(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.HTTP.MaxBodyBytes = 4
	var statuses []int
	var requestIDs []string
	instrument := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestIDs = append(requestIDs, web.RequestID(r.Context()))
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			statuses = append(statuses, recorder.Code)
			w.WriteHeader(recorder.Code)
		})
	}
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	server, err := httpserver.NewWithOptions(cfg, panicking, slog.New(slog.NewTextHandler(io.Discard, nil)), httpserver.Options{Instrument: instrument})
	if err != nil {
		t.Fatal(err)
	}
	server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader("too long")))
	if !slices.Equal(statuses, []int{http.StatusInternalServerError, http.StatusRequestEntityTooLarge}) {
		t.Fatalf("instrumentation saw %v", statuses)
	}
	for _, id := range requestIDs {
		if id == "" {
			t.Fatal("instrumentation must run inside the request-ID layer")
		}
	}
	if _, err := httpserver.NewWithOptions(cfg, panicking, slog.New(slog.NewTextHandler(io.Discard, nil)), httpserver.Options{
		Instrument: func(http.Handler) http.Handler { return nil },
	}); err == nil {
		t.Fatal("a nil instrumented handler must be rejected")
	}
}

func TestServerResolvesClientIPFromTrustedProxiesOnly(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.HTTP.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	var seen, seenByMiddleware, seenByInstrument string
	server, err := httpserver.NewWithOptions(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = web.ClientIP(r)
		w.WriteHeader(http.StatusNoContent)
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), httpserver.Options{
		Instrument: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenByInstrument = web.ClientIP(r)
				next.ServeHTTP(w, r)
			})
		},
		Middlewares: []httpserver.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenByMiddleware = web.ClientIP(r)
				next.ServeHTTP(w, r)
			})
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ remote, xff, want string }{
		{"172.18.0.2:5000", "203.0.113.9", "203.0.113.9"},
		{"198.51.100.7:4000", "203.0.113.9", "198.51.100.7"},
		{"172.18.0.2:5000", "", "172.18.0.2"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = tc.remote
		if tc.xff != "" {
			request.Header.Set("X-Forwarded-For", tc.xff)
		}
		server.Handler().ServeHTTP(httptest.NewRecorder(), request)
		if seen != tc.want || seenByMiddleware != tc.want || seenByInstrument != tc.want {
			t.Fatalf("%s with X-Forwarded-For %q: handler %q, middleware %q, instrument %q, want %q", tc.remote, tc.xff, seen, seenByMiddleware, seenByInstrument, tc.want)
		}
	}
}

func TestServerRejectsUnsafeTrustedProxies(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.HTTP.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	if _, err := httpserver.New(cfg, http.NotFoundHandler(), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("a server trusting every address was built")
	}
}
