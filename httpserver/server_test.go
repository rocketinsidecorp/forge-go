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

	"github.com/fabriciobonjorno/forge-go/config"
	"github.com/fabriciobonjorno/forge-go/httpserver"
	"github.com/fabriciobonjorno/forge-go/web"
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

func TestTrustedProxyUsesForwardedValuesOnlyFromAllowlistedPeers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		remoteAddr string
		forwarded  string
		proto      string
		wantIP     string
		wantScheme string
	}{
		{
			name:       "trusted chain",
			remoteAddr: "10.0.0.2:4321",
			forwarded:  "198.51.100.9, 10.0.0.3",
			proto:      "https",
			wantIP:     "198.51.100.9",
			wantScheme: "https",
		},
		{
			name:       "untrusted peer cannot spoof metadata",
			remoteAddr: "203.0.113.10:5555",
			forwarded:  "198.51.100.9",
			proto:      "https",
			wantIP:     "203.0.113.10",
			wantScheme: "http",
		},
		{
			name:       "malformed headers are ignored",
			remoteAddr: "10.0.0.2:4321",
			forwarded:  "198.51.100.9, garbage",
			proto:      "https, http",
			wantIP:     "10.0.0.2",
			wantScheme: "http",
		},
		{
			name:       "oversized forwarded chain is ignored",
			remoteAddr: "10.0.0.2:4321",
			forwarded:  strings.Repeat("198.51.100.9,", 400),
			proto:      "https",
			wantIP:     "10.0.0.2",
			wantScheme: "https",
		},
		{
			name:       "trusted IPv6 proxy",
			remoteAddr: "[2001:db8:1::2]:4321",
			forwarded:  "2001:db8:2::9",
			proto:      "https",
			wantIP:     "2001:db8:2::9",
			wantScheme: "https",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.HTTP.TrustedProxyCIDRs = []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("2001:db8:1::/48"),
			}
			var clientIP, scheme string
			server, err := httpserver.NewWithMiddleware(
				cfg,
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					if ip, ok := web.ClientIP(r); ok {
						clientIP = ip.String()
					}
					scheme = web.RequestScheme(r)
				}),
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = test.remoteAddr
			request.Header.Set("X-Forwarded-For", test.forwarded)
			request.Header.Set("X-Forwarded-Proto", test.proto)
			server.Handler().ServeHTTP(httptest.NewRecorder(), request)
			if clientIP != test.wantIP || scheme != test.wantScheme {
				t.Fatalf("client IP=%q scheme=%q, want IP=%q scheme=%q", clientIP, scheme, test.wantIP, test.wantScheme)
			}
		})
	}
}

func TestUnconfiguredProxyHeadersAreIgnored(t *testing.T) {
	t.Parallel()
	var clientIP, scheme string
	server, err := httpserver.NewWithMiddleware(
		config.Default(),
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if ip, ok := web.ClientIP(r); ok {
				clientIP = ip.String()
			}
			scheme = web.RequestScheme(r)
		}),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "203.0.113.10:5555"
	request.Header.Set("X-Forwarded-For", "198.51.100.9")
	request.Header.Set("X-Forwarded-Proto", "https")
	server.Handler().ServeHTTP(httptest.NewRecorder(), request)
	if clientIP != "203.0.113.10" || scheme != "http" {
		t.Fatalf("client IP=%q scheme=%q, want socket peer and http", clientIP, scheme)
	}
}
