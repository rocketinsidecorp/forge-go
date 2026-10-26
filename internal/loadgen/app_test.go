package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeApp imitates the routes of a generated application that newTarget and
// the scenarios touch. override replaces the handler of one "METHOD /path".
type fakeApp struct {
	mu       sync.Mutex
	hits     map[string]int
	forms    []map[string]string
	bearer   string
	cookies  []string
	override map[string]http.HandlerFunc
}

func newFakeApp(t *testing.T, override map[string]http.HandlerFunc) (*fakeApp, string) {
	t.Helper()
	app := &fakeApp{hits: map[string]int{}, override: override}
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return app, server.URL
}

func (a *fakeApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	a.mu.Lock()
	a.hits[key]++
	a.mu.Unlock()
	if h, ok := a.override[key]; ok {
		h(w, r)
		return
	}
	switch key {
	case "GET /health/ready", "GET /health", "GET /":
	case "GET /session/new":
		http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "csrf-value", Path: "/", Secure: true})
		_, _ = w.Write([]byte(`<form><input type="hidden" name="authenticity_token" value="form-token">`))
	case "POST /session":
		_ = r.ParseForm()
		a.mu.Lock()
		a.forms = append(a.forms, map[string]string{
			"token": r.PostForm.Get("authenticity_token"), "tenant": r.PostForm.Get("tenant"),
			"email": r.PostForm.Get("email"), "password": r.PostForm.Get("password"), "cookie": r.Header.Get("Cookie"),
		})
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "session-value", Path: "/", Secure: true})
		w.WriteHeader(http.StatusSeeOther)
	case "POST /auth/login":
		_, _ = w.Write([]byte(`{"access_token":"bearer-token"}`))
	case "POST /posts":
		_ = r.ParseForm()
		a.mu.Lock()
		a.forms = append(a.forms, map[string]string{"create-token": r.PostForm.Get("authenticity_token"), "cookie": r.Header.Get("Cookie")})
		a.mu.Unlock()
		w.Header().Set("Location", "/posts/0190-abc")
		w.WriteHeader(http.StatusSeeOther)
	case "GET /posts":
		a.mu.Lock()
		a.cookies = append(a.cookies, r.Header.Get("Cookie")+"|"+r.URL.RawQuery)
		a.mu.Unlock()
	case "GET /v1/posts/0190-abc":
		a.mu.Lock()
		a.bearer = r.Header.Get("Authorization")
		a.mu.Unlock()
	default:
		http.NotFound(w, r)
	}
}

func testConfig(base string) config {
	return config{
		base: base, collection: "posts", tenant: "acme", email: "owner@example.com", password: "s3cret",
		concurrency: 2, duration: 20 * time.Millisecond, warmup: 5 * time.Millisecond, runs: 2, rows: 3,
		scenarios: allScenarios, format: "markdown",
	}
}

func TestRunMeasuresEveryScenarioAndReports(t *testing.T) {
	t.Setenv("BENCH_PASSWORD", "s3cret")
	// Each window is long enough for a loaded machine to finish a request;
	// the JSON run repeats so that medians are over several runs.
	for format, runs := range map[string]string{"markdown": "1", "json": "2"} {
		app, base := newFakeApp(t, nil)
		var stdout, stderr bytes.Buffer
		err := run([]string{
			"-base", base + "/", "-tenant", "acme", "-email", "owner@example.com", "-concurrency", "2",
			"-duration", "150ms", "-warmup", "50ms", "-runs", runs, "-rows", "3", "-format", format,
		}, &stdout, &stderr)
		if err != nil {
			t.Fatalf("%s: run: %v\nstderr: %s", format, err, stderr.String())
		}
		for _, name := range allScenarios {
			if !strings.Contains(stdout.String(), name) {
				t.Errorf("%s: report lacks scenario %q:\n%s", format, name, stdout.String())
			}
			if !strings.Contains(stderr.String(), "warm-up "+name) || !strings.Contains(stderr.String(), "run "+runs+"/"+runs+" "+name) {
				t.Errorf("%s: progress lacks %q:\n%s", format, name, stderr.String())
			}
		}
		if format == "json" {
			var decoded struct {
				Concurrency int      `json:"concurrency"`
				Runs        int      `json:"runs"`
				Results     []result `json:"results"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
				t.Fatalf("report is not JSON: %v\n%s", err, stdout.String())
			}
			if decoded.Concurrency != 2 || decoded.Runs != 2 || len(decoded.Results) != len(allScenarios) {
				t.Fatalf("decoded = %+v", decoded)
			}
			for _, r := range decoded.Results {
				if len(r.Runs) != 2 || r.Median.RequestsPerSecond <= 0 || r.Errors != 0 {
					t.Errorf("result %s = %+v", r.Scenario, r)
				}
			}
		} else if !strings.Contains(stdout.String(), "| Scenario | Requests/s |") {
			t.Errorf("no markdown table:\n%s", stdout.String())
		}

		app.mu.Lock()
		sign := app.forms[0]
		if sign["token"] != "form-token" || sign["tenant"] != "acme" || sign["email"] != "owner@example.com" || sign["password"] != "s3cret" || sign["cookie"] != csrfCookie+"=csrf-value" {
			t.Errorf("%s: sign-in form = %v", format, sign)
		}
		if create := app.forms[1]; create["create-token"] != "csrf-value" || !strings.Contains(create["cookie"], sessionCookie+"=session-value") {
			t.Errorf("%s: create request = %v", format, create)
		}
		if app.bearer != "Bearer bearer-token" {
			t.Errorf("%s: show_json authorization = %q", format, app.bearer)
		}
		if len(app.cookies) == 0 || app.cookies[0] != sessionCookie+"=session-value; "+csrfCookie+"=csrf-value|limit=3" {
			t.Errorf("%s: index_html request = %v", format, app.cookies)
		}
		app.mu.Unlock()
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	t.Setenv("BENCH_PASSWORD", "")
	if err := run([]string{"-no-such-flag"}, &stdout, &stderr); err == nil {
		t.Error("an unknown flag must fail")
	}
	if err := run(nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "BENCH_PASSWORD") {
		t.Errorf("missing password: %v", err)
	}
	t.Setenv("BENCH_PASSWORD", "x")
	if err := run([]string{"-scenarios", "health,bogus"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), `unknown scenario "bogus"`) {
		t.Errorf("unknown scenario: %v", err)
	}
}

func TestRunReportsASignInFailure(t *testing.T) {
	t.Setenv("BENCH_PASSWORD", "x")
	_, base := newFakeApp(t, map[string]http.HandlerFunc{
		"GET /session/new": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
	})
	var stdout, stderr bytes.Buffer
	err := run([]string{"-base", base}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "open the sign-in page") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunStopsWhenMeasurementFails(t *testing.T) {
	t.Setenv("BENCH_PASSWORD", "x")
	_, base := newFakeApp(t, map[string]http.HandlerFunc{
		"GET /health": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
	})
	var stdout, stderr bytes.Buffer
	err := run([]string{"-base", base, "-concurrency", "1", "-duration", "20ms", "-warmup", "0", "-runs", "1", "-rows", "1", "-scenarios", "health"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "health: no request succeeded") {
		t.Fatalf("err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("a failed run must not print a report: %q", stdout.String())
	}
}

func TestNewTargetNamesTheStepThatFailed(t *testing.T) {
	t.Parallel()
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
	}
	body := func(s string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(s)) }
	}
	for _, tc := range []struct {
		name     string
		override map[string]http.HandlerFunc
		want     string
	}{
		{"sign-in page unavailable", map[string]http.HandlerFunc{"GET /session/new": status(http.StatusInternalServerError)}, "open the sign-in page"},
		{"no csrf cookie", map[string]http.HandlerFunc{"GET /session/new": body(`name="authenticity_token" value="t"`)}, "no CSRF cookie and token"},
		{"no csrf token", map[string]http.HandlerFunc{"GET /session/new": func(w http.ResponseWriter, _ *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "c"})
		}}, "no CSRF cookie and token"},
		{"sign-in refused", map[string]http.HandlerFunc{"POST /session": status(http.StatusUnauthorized)}, "sign in"},
		{"no session cookie", map[string]http.HandlerFunc{"POST /session": status(http.StatusSeeOther)}, "no session cookie"},
		{"login refused", map[string]http.HandlerFunc{"POST /auth/login": status(http.StatusForbidden)}, "log in for a bearer token"},
		{"login answer is not json", map[string]http.HandlerFunc{"POST /auth/login": body("nope")}, "no access token"},
		{"login answer has no token", map[string]http.HandlerFunc{"POST /auth/login": body("{}")}, "no access token"},
		{"create refused", map[string]http.HandlerFunc{"POST /posts": status(http.StatusUnprocessableEntity)}, "create a row"},
		{"create without location", map[string]http.HandlerFunc{"POST /posts": status(http.StatusSeeOther)}, "unexpected Location"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, base := newFakeApp(t, tc.override)
			target, err := newTarget(context.Background(), testConfig(base))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q (target %v)", err, tc.want, target)
			}
		})
	}
}

func TestWaitReady(t *testing.T) {
	t.Parallel()
	t.Run("retries until the application is ready", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		calls := 0
		app, base := newFakeApp(t, map[string]http.HandlerFunc{
			"GET /health/ready": func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if calls++; calls == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			},
		})
		tg := &target{client: http.DefaultClient, base: base}
		if err := tg.waitReady(context.Background()); err != nil {
			t.Fatal(err)
		}
		if app.hits["GET /health/ready"] != 2 {
			t.Errorf("readiness polled %d times, want 2", app.hits["GET /health/ready"])
		}
	})
	t.Run("gives up when the context is done", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, base := newFakeApp(t, map[string]http.HandlerFunc{
			"GET /health/ready": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		})
		tg := &target{client: http.DefaultClient, base: base}
		err := tg.waitReady(ctx)
		if err == nil || !strings.Contains(err.Error(), "is not ready at "+base) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDo(t *testing.T) {
	t.Parallel()
	_, base := newFakeApp(t, map[string]http.HandlerFunc{
		"GET /big": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("a", 2<<20)))
		},
		"GET /truncated": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("short"))
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		},
		"GET /teapot": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("short and stout"))
		},
	})
	tg := &target{client: &http.Client{Timeout: 5 * time.Second}, base: base}
	ctx := context.Background()

	status, _, got, err := tg.do(ctx, request{method: http.MethodGet, path: "/big", want: http.StatusOK})
	if err != nil || status != 200 || len(got) != 1<<20 {
		t.Errorf("a body is capped at 1 MiB: status %d, len %d, err %v", status, len(got), err)
	}
	status, header, got, err := tg.do(ctx, request{method: http.MethodGet, path: "/teapot", want: http.StatusOK})
	if err == nil || status != http.StatusTeapot || got != "short and stout" || header == nil ||
		!strings.Contains(err.Error(), "GET /teapot answered 418, want 200") {
		t.Errorf("unexpected status: %d %q %v", status, got, err)
	}
	if _, _, _, err := tg.do(ctx, request{method: http.MethodGet, path: "/truncated", want: http.StatusOK}); err == nil {
		t.Error("a truncated body must fail")
	}
	if _, _, _, err := tg.do(ctx, request{method: "BAD METHOD", path: "/", want: 200}); err == nil {
		t.Error("an invalid method must fail to build")
	}
	if _, _, _, err := (&target{client: http.DefaultClient, base: "http://127.0.0.1:1"}).do(ctx, request{method: http.MethodGet, path: "/", want: 200}); err == nil {
		t.Error("a refused connection must fail")
	}
}

func TestRequestBuildClonesHeadersAndBody(t *testing.T) {
	t.Parallel()
	r := request{method: http.MethodPost, path: "/x", body: "a=b", header: http.Header{"Cookie": {"c=1"}}}
	built, err := r.build(context.Background(), "http://host")
	if err != nil {
		t.Fatal(err)
	}
	built.Header["Cookie"][0] = "mutated"
	if r.header["Cookie"][0] != "c=1" {
		t.Errorf("build shares the header slice with the scenario: %v", r.header)
	}
	if built.URL.String() != "http://host/x" || built.Body == nil {
		t.Errorf("built %v body %v", built.URL, built.Body)
	}
	empty, err := request{method: http.MethodGet, path: "/y"}.build(context.Background(), "http://host")
	if err != nil || empty.Body != nil {
		t.Errorf("a request without a body has none: %v %v", empty.Body, err)
	}
}

func TestCookieValue(t *testing.T) {
	t.Parallel()
	header := http.Header{"Set-Cookie": {"a=1; Path=/", "b=2"}}
	for name, want := range map[string]string{"a": "1", "b": "2", "c": ""} {
		if got := cookieValue(header, name); got != want {
			t.Errorf("cookieValue(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDriveCountsOutcomes(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	n := 0
	enough := make(chan struct{})
	_, base := newFakeApp(t, map[string]http.HandlerFunc{
		"GET /flaky": func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			n++
			odd, seen := n%2 == 1, n
			mu.Unlock()
			if seen == 12 {
				close(enough)
			}
			if odd {
				w.WriteHeader(http.StatusInternalServerError)
			}
		},
	})
	tg := &target{client: &http.Client{Timeout: 5 * time.Second}, base: base}
	// The window is far longer than needed; it ends when the application has
	// seen enough requests, so the outcome does not depend on the speed of
	// the machine.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-enough; cancel() }()
	s := tg.drive(ctx, request{method: http.MethodGet, path: "/flaky", want: 200}, 2, time.Minute)
	if s.ok == 0 || s.errors == 0 || s.ok != len(s.latencies) || s.elapsed <= 0 {
		t.Fatalf("sample = ok %d errors %d latencies %d elapsed %v", s.ok, s.errors, len(s.latencies), s.elapsed)
	}
	for i := 1; i < len(s.latencies); i++ {
		if s.latencies[i-1] > s.latencies[i] {
			t.Fatal("latencies are not sorted")
		}
	}
}

func TestMeasureReturnsTheContextErrorWhenCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testConfig("http://unused")
	cfg.warmup = 0
	cfg.scenarios = []string{scenarioHealth}
	results, err := measure(ctx, cfg, &target{}, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) || results != nil {
		t.Fatalf("results %v err %v", results, err)
	}
}

func TestReportMarkdownShowsTheRangeOfRuns(t *testing.T) {
	t.Parallel()
	runs := []stats{{RequestsPerSecond: 100}, {RequestsPerSecond: 300}, {RequestsPerSecond: 200}}
	var out bytes.Buffer
	cfg := config{concurrency: 1, duration: time.Second, runs: 3, rows: 5, format: "markdown"}
	if err := report(&out, cfg, []result{{Scenario: scenarioWelcome, Runs: runs, Median: medianOf(runs), Errors: 4}}); err != nil {
		t.Fatal(err)
	}
	if want := "| `welcome` | 200 | 100-300 | 0.00 | 0.00 | 0.00 | 4 |"; !strings.Contains(out.String(), want) {
		t.Fatalf("report lacks %q:\n%s", want, out.String())
	}
}

func TestMillisecondFormatting(t *testing.T) {
	t.Parallel()
	for v, want := range map[float64]string{0: "0.00", 9.994: "9.99", 10: "10.0", 123.456: "123.5"} {
		if got := ms(v); got != want {
			t.Errorf("ms(%v) = %q, want %q", v, got, want)
		}
	}
	if got := statsOf(sample{ok: 10, elapsed: time.Second, latencies: []time.Duration{time.Millisecond}}); got.RequestsPerSecond != 10 || got.P50Ms != 1 {
		t.Errorf("statsOf = %+v", got)
	}
}
