package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

// steppingClock advances by step at every reading, so durations are exact.
type steppingClock struct {
	now  time.Time
	step time.Duration
}

func (c *steppingClock) Now() time.Time {
	c.now = c.now.Add(c.step)
	return c.now
}

// serveAccessLog serves one request through the access log, under a request
// logger that carries a request ID, and returns the decoded log lines.
func serveAccessLog(t *testing.T, opts AccessLogOptions, request *http.Request, handler http.Handler) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})).With("request_id", "req-1")
	ctx := web.WithLogger(web.WithRequestID(request.Context(), "req-1"), logger)
	recorder := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		AccessLog(opts)(handler).ServeHTTP(recorder, request.WithContext(ctx))
	}()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, record)
	}
	return lines
}

// routed answers like the router: it records the matched pattern.
func routed(pattern string, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		web.RecordRoute(r.Context(), pattern)
		w.WriteHeader(status)
	})
}

func TestAccessLogWritesTheRouteNotThePath(t *testing.T) {
	clock := &steppingClock{now: prettyTime, step: 4100 * time.Microsecond}
	request := httptest.NewRequest(http.MethodGet, "/posts/zz-record-id?email=ana@example.com&token=tok-leak-1", nil)
	lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo, Now: clock.Now}, request, routed("GET /posts/{id}", http.StatusOK))
	if len(lines) != 1 {
		t.Fatalf("%d lines", len(lines))
	}
	line := lines[0]
	if line["msg"] != LogEventHTTP || line["level"] != "INFO" || line["method"] != "GET" || line["route"] != "/posts/{id}" ||
		line["status"] != float64(200) || line["duration"] != float64(4100000) || line["request_id"] != "req-1" {
		t.Fatalf("%v", line)
	}
	if _, ok := line["queries"]; ok {
		t.Fatalf("queries without CountQueries: %v", line)
	}
	// Neither the path, nor the query string, nor the client's headers.
	encoded, _ := json.Marshal(line)
	for _, leak := range []string{"zz-record-id", "ana@example.com", "tok-leak-1", "email=", "?"} {
		if strings.Contains(string(encoded), leak) {
			t.Errorf("the line contains %q: %s", leak, encoded)
		}
	}
}

func TestAccessLogStatusesMethodsAndUnmatchedRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, pattern string
		status          int
		route, want     string
	}{
		{http.MethodPost, "POST /posts", http.StatusCreated, "/posts", "POST"},
		{http.MethodGet, "", http.StatusNotFound, "", "GET"},
		{"BREW", "", http.StatusTeapot, "", "_OTHER"},
		{http.MethodDelete, "DELETE /posts/{id}", http.StatusInternalServerError, "/posts/{id}", "DELETE"},
	} {
		handler := routed(tc.pattern, tc.status)
		if tc.pattern == "" {
			handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) })
		}
		lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo}, httptest.NewRequest(tc.method, "/x/secret-id", nil), handler)
		if len(lines) != 1 || lines[0]["route"] != tc.route || lines[0]["status"] != float64(tc.status) || lines[0]["method"] != tc.want {
			t.Errorf("%s %s: %v", tc.method, tc.pattern, lines)
		}
	}
	// A handler that writes a body without a status answers 200.
	lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo}, httptest.NewRequest(http.MethodGet, "/", nil),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	if lines[0]["status"] != float64(200) {
		t.Fatalf("%v", lines[0])
	}
}

func TestAccessLogCountsQueries(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 3 {
			CountQuery(r.Context())
		}
		w.WriteHeader(http.StatusOK)
	})
	lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo, CountQueries: true}, httptest.NewRequest(http.MethodGet, "/", nil), handler)
	if lines[0]["queries"] != float64(3) {
		t.Fatalf("%v", lines[0])
	}
	// A request that runs none says 0: that is the point of the count.
	lines = serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo, CountQueries: true}, httptest.NewRequest(http.MethodGet, "/", nil), routed("GET /", 200))
	if lines[0]["queries"] != float64(0) {
		t.Fatalf("%v", lines[0])
	}
}

func TestAccessLogLevel(t *testing.T) {
	// Production: Debug, which the logger at Info does not write, and the
	// request is served without a counter or a recorder.
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(web.WithLogger(request.Context(), logger))
	served := false
	AccessLog(AccessLogOptions{Level: slog.LevelDebug, CountQueries: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		if _, ok := w.(*statusRecorder); ok || web.RouteInfoFromContext(r.Context()) != nil {
			t.Error("a disabled access log must not wrap the request")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), request)
	if !served || out.Len() != 0 {
		t.Fatalf("served %t, logged %q", served, out.String())
	}
	// FORGE_LOG_LEVEL=debug turns it on at Debug.
	lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelDebug}, httptest.NewRequest(http.MethodGet, "/", nil), routed("GET /", 200))
	if len(lines) != 1 || lines[0]["level"] != "DEBUG" {
		t.Fatalf("%v", lines)
	}
}

func TestAccessLogReusesTheRouteInfoOfTheOuterMiddleware(t *testing.T) {
	outer := &web.RouteInfo{}
	request := httptest.NewRequest(http.MethodGet, "/posts/1", nil)
	request = request.WithContext(web.WithRouteInfo(request.Context(), outer))
	lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo}, request, routed("GET /posts/{id}", 200))
	if lines[0]["route"] != "/posts/{id}" || outer.Pattern() != "GET /posts/{id}" {
		t.Fatalf("route %v, outer %q: the telemetry middleware must still see the route", lines[0]["route"], outer.Pattern())
	}
}

func TestAccessLogLogsAPanicAsAServerError(t *testing.T) {
	lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo}, httptest.NewRequest(http.MethodGet, "/", nil),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	if len(lines) != 1 || lines[0]["status"] != float64(500) {
		t.Fatalf("%v", lines)
	}
	// The panic goes on to whoever recovers it.
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	defer func() {
		if recover() != "boom" {
			t.Fatal("the panic must propagate")
		}
	}()
	AccessLog(AccessLogOptions{Level: slog.LevelInfo})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).
		ServeHTTP(httptest.NewRecorder(), request.WithContext(context.Background()))
}

func TestAccessLogUsesTheWallClockByDefault(t *testing.T) {
	lines := serveAccessLog(t, AccessLogOptions{Level: slog.LevelInfo}, httptest.NewRequest(http.MethodGet, "/", nil), routed("GET /", 200))
	if d, ok := lines[0]["duration"].(float64); !ok || d < 0 {
		t.Fatalf("%v", lines[0])
	}
}
