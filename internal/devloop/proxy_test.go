package devloop

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const page = "<!doctype html><html><head><title>t</title></head><body><h1>Hi</h1></body></html>"

// fixture is a proxy in front of a configurable application.
type fixture struct {
	proxy    *Proxy
	server   *httptest.Server
	upstream *httptest.Server
	hits     atomic.Int32
	last     atomic.Pointer[http.Request]
}

func newFixture(t *testing.T, app http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{}
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.last.Store(r.Clone(context.Background()))
		app(w, r)
	}))
	t.Cleanup(f.upstream.Close)
	proxy, err := NewProxy(ProxyOptions{Environment: "development", ShowBuildErrors: true})
	if err != nil {
		t.Fatal(err)
	}
	f.proxy = proxy
	proxy.SetUpstream(strings.TrimPrefix(f.upstream.URL, "http://"))
	f.server = httptest.NewServer(proxy.Handler())
	t.Cleanup(func() { proxy.Close(); f.server.Close() })
	return f
}

func (f *fixture) get(t *testing.T, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

func servePage(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}
}

func TestNewProxyRefusesEveryEnvironmentButDevelopment(t *testing.T) {
	t.Parallel()
	for _, env := range []string{"", "production", "test", "staging", "Development"} {
		if _, err := NewProxy(ProxyOptions{Environment: env}); err == nil {
			t.Errorf("NewProxy accepted FORGE_ENV=%q", env)
		}
	}
}

func TestProxyInjectsTheScriptIntoHTMLPagesOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		body        string
		inject      bool
	}{
		{"html page", "text/html; charset=utf-8", page, true},
		{"uppercase closing tag", "text/html", "<html><BODY>x</BODY></html>", true},
		{"json", "application/json", `{"html":"</body>"}`, false},
		{"plain text", "text/plain", "</body>", false},
		{"javascript", "text/javascript", "// </body>", false},
		{"turbo stream", "text/vnd.turbo-stream.html", "<turbo-stream></turbo-stream></body>", false},
		{"html fragment without a body", "text/html", "<li>partial</li>", false},
		{"svg", "image/svg+xml", "<svg></svg></body>", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			})
			response, body := f.get(t, "/", nil)
			got := strings.Contains(body, scriptPath)
			if got != test.inject {
				t.Fatalf("injected = %v, want %v\n%s", got, test.inject, body)
			}
			if !test.inject {
				if body != test.body {
					t.Fatalf("body changed: %q", body)
				}
				return
			}
			if length, _ := strconv.Atoi(response.Header.Get("Content-Length")); length != len(body) {
				t.Errorf("Content-Length = %q, body is %d bytes", response.Header.Get("Content-Length"), len(body))
			}
			if response.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
			}
			if idx, tag := strings.Index(strings.ToLower(body), "</body"), strings.Index(body, "<script"); tag < 0 || tag > idx {
				t.Errorf("script must come before </body>:\n%s", body)
			}
		})
	}
}

func TestProxyLeavesNonPageResponsesAlone(t *testing.T) {
	t.Parallel()
	t.Run("compressed", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, "not really gzip </body>")
		})
		if _, body := f.get(t, "/", nil); strings.Contains(body, scriptPath) {
			t.Fatal("injected into a compressed body")
		}
	})
	t.Run("attachment", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Disposition", `attachment; filename="r.html"`)
			_, _ = io.WriteString(w, page)
		})
		if _, body := f.get(t, "/", nil); body != page {
			t.Fatalf("attachment modified: %q", body)
		}
	})
	t.Run("not modified", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotModified)
		})
		if response, _ := f.get(t, "/", nil); response.StatusCode != http.StatusNotModified {
			t.Fatalf("status = %d", response.StatusCode)
		}
	})
	t.Run("too large to buffer", func(t *testing.T) {
		t.Parallel()
		big := page + strings.Repeat("x", 200)
		f := newFixture(t, servePage(big))
		f.proxy.opts.MaxInjectBytes = 100
		if _, body := f.get(t, "/", nil); body != big {
			t.Fatalf("an oversized page must pass through unchanged, got %d bytes", len(body))
		}
	})
	t.Run("already injected", func(t *testing.T) {
		t.Parallel()
		once := "<body>" + `<script src="` + scriptPath + `"></script></body>`
		f := newFixture(t, servePage(once))
		if _, body := f.get(t, "/", nil); body != once {
			t.Fatalf("injected twice: %q", body)
		}
	})
}

func TestProxyHonorsTheApplicationsContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	nonceOf := regexp.MustCompile(`<script src="/__forge/reload.js" nonce="([^"]+)"`)

	t.Run("forge's default policy is left as it is", func(t *testing.T) {
		t.Parallel()
		const policy = "default-src 'none'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'"
		f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Security-Policy", policy)
			_, _ = io.WriteString(w, page)
		})
		response, _ := f.get(t, "/", nil)
		if got := response.Header.Get("Content-Security-Policy"); got != policy {
			t.Fatalf("CSP = %q, want it unchanged", got)
		}
	})

	t.Run("a strict policy gets the page's nonce and nothing broader", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Add("Content-Security-Policy", "default-src 'none'")
			w.Header().Add("Content-Security-Policy", "script-src 'nonce-app'; object-src 'none'")
			_, _ = io.WriteString(w, page)
		})
		response, body := f.get(t, "/", nil)
		match := nonceOf.FindStringSubmatch(body)
		if match == nil {
			t.Fatalf("no nonce on the script tag:\n%s", body)
		}
		nonce := match[1]
		policies := response.Header.Values("Content-Security-Policy")
		if len(policies) != 2 {
			t.Fatalf("every policy header must be kept, got %v", policies)
		}
		want := []string{
			"default-src 'none'; script-src-elem 'nonce-" + nonce + "'; connect-src 'self'",
			"script-src 'nonce-app' 'nonce-" + nonce + "'; object-src 'none'",
		}
		for i := range want {
			if policies[i] != want[i] {
				t.Errorf("policy %d = %q, want %q", i, policies[i], want[i])
			}
		}
		for _, policy := range policies {
			if strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "unsafe-eval") {
				t.Errorf("policy %q was weakened", policy)
			}
		}
	})

	t.Run("each response gets a fresh nonce", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, servePage(page))
		_, first := f.get(t, "/", nil)
		_, second := f.get(t, "/", nil)
		a, b := nonceOf.FindStringSubmatch(first), nonceOf.FindStringSubmatch(second)
		if a == nil || b == nil || a[1] == b[1] {
			t.Fatalf("nonces %v and %v must differ", a, b)
		}
	})
}

func TestProxyForwardsRequestsFaithfully(t *testing.T) {
	t.Parallel()
	f := newFixture(t, servePage(page))
	response, _ := f.get(t, "/a//b/../c?x=1&y=%2F", http.Header{"Accept-Encoding": {"gzip"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: unclean paths must be forwarded, not redirected", response.StatusCode)
	}
	got := f.last.Load()
	if got.URL.RequestURI() != "/a//b/../c?x=1&y=%2F" {
		t.Errorf("upstream saw %q", got.URL.RequestURI())
	}
	if want := strings.TrimPrefix(f.server.URL, "http://"); got.Host != want {
		t.Errorf("upstream Host = %q, want the browser's %q", got.Host, want)
	}
	if got.Header.Get("Accept-Encoding") != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity so pages can be rewritten", got.Header.Get("Accept-Encoding"))
	}
	if got.Header.Get("X-Forwarded-For") == "" {
		t.Error("X-Forwarded-For is missing")
	}
}

func TestProxyAnswersReservedPathsItselfAndNeverForwardsThem(t *testing.T) {
	t.Parallel()
	f := newFixture(t, servePage(page))
	response, body := f.get(t, scriptPath, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "EventSource") {
		t.Fatalf("script: %d %q", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("Content-Type = %q", got)
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", response.Header)
	}
	if response, _ := f.get(t, ReservedPrefix+"other", nil); response.StatusCode != http.StatusNotFound {
		t.Errorf("unknown reserved path = %d, want 404", response.StatusCode)
	}
	if f.hits.Load() != 0 {
		t.Errorf("%d reserved requests reached the application", f.hits.Load())
	}
}

func TestProxyShowsAWaitPageWithoutAnApplication(t *testing.T) {
	t.Parallel()
	f := newFixture(t, servePage(page))
	f.proxy.Restarting()
	response, body := f.get(t, "/anything", nil)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Restarting the application") || !strings.Contains(body, scriptPath) {
		t.Fatalf("wait page: %d\n%s", response.StatusCode, body)
	}
	if f.hits.Load() != 0 {
		t.Fatal("request reached a stopped application")
	}
	nonce := regexp.MustCompile(`nonce="([^"]+)"`).FindStringSubmatch(body)
	csp := response.Header.Get("Content-Security-Policy")
	if nonce == nil || !strings.Contains(csp, "'nonce-"+nonce[1]+"'") || strings.Contains(csp, "unsafe") {
		t.Errorf("wait page CSP %q does not match its nonce %v", csp, nonce)
	}

	f.proxy.BuildFailed("main.go:1: <script>alert(1)</script> & more")
	f.proxy.Unavailable("Build failed")
	_, body = f.get(t, "/", nil)
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;alert(1)") {
		t.Fatalf("build output must be HTML-escaped:\n%s", body)
	}

	f.proxy.SetUpstream(strings.TrimPrefix(f.upstream.URL, "http://"))
	if response, _ := f.get(t, "/", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("status after SetUpstream = %d", response.StatusCode)
	}
}

func TestProxyReportsAnUnreachableApplication(t *testing.T) {
	t.Parallel()
	proxy, err := NewProxy(ProxyOptions{Environment: "development"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	_ = l.Close()
	proxy.SetUpstream(dead)
	server := httptest.NewServer(proxy.Handler())
	t.Cleanup(server.Close)
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
}

// stream opens the event stream and returns a reader of its events.
type stream struct {
	events chan [2]string
	cancel context.CancelFunc
	status int
	done   chan struct{}
}

func openStream(t *testing.T, base, host string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+eventsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		request.Host = host
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{events: make(chan [2]string, 32), cancel: cancel, status: response.StatusCode, done: make(chan struct{})}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		close(s.done)
		return s
	}
	go func() {
		defer close(s.done)
		defer func() { _ = response.Body.Close() }()
		scanner := bufio.NewScanner(response.Body)
		var name string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				s.events <- [2]string{name, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return s
}

func (s *stream) next(t *testing.T) [2]string {
	t.Helper()
	select {
	case e := <-s.events:
		return e
	case <-time.After(testWait):
		t.Fatal("no event")
		return [2]string{}
	}
}

func TestProxyStreamsReloadEventsToBrowsers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, servePage(page))
	a, b := openStream(t, f.server.URL, ""), openStream(t, f.server.URL, "")
	for _, s := range []*stream{a, b} {
		if hello := s.next(t); hello[0] != "hello" || hello[1] != f.proxy.bootID {
			t.Fatalf("first event = %v", hello)
		}
	}
	f.proxy.Reload()
	for _, s := range []*stream{a, b} {
		if e := s.next(t); e[0] != "reload" {
			t.Fatalf("event = %v, want reload", e)
		}
	}
}

func TestProxyShowsBuildErrorsOnlyToLoopbackHosts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, servePage(page))
	loopback := openStream(t, f.server.URL, "")
	remote := openStream(t, f.server.URL, "dev.example.com:8080")
	loopback.next(t)
	remote.next(t)
	f.proxy.BuildFailed("main.go:9: secret/path/main.go undefined: x")

	if e := loopback.next(t); e[0] != "build-error" || !strings.Contains(e[1], "undefined: x") {
		t.Fatalf("loopback event = %v", e)
	}
	e := remote.next(t)
	if e[0] != "build-error" || strings.Contains(e[1], "undefined") || strings.Contains(e[1], "secret") {
		t.Fatalf("a non-loopback Host must not receive compiler output: %v", e)
	}

	// A browser that connects later sees the standing error.
	late := openStream(t, f.server.URL, "")
	late.next(t) // hello
	if e := late.next(t); e[0] != "build-error" || !strings.Contains(e[1], "undefined: x") {
		t.Fatalf("late event = %v", e)
	}
}

func TestProxyHidesBuildErrorsWhenNotLoopbackBound(t *testing.T) {
	t.Parallel()
	proxy, err := NewProxy(ProxyOptions{Environment: "development", ShowBuildErrors: false})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy.Handler())
	t.Cleanup(func() { proxy.Close(); server.Close() })
	s := openStream(t, server.URL, "")
	s.next(t)
	proxy.BuildFailed("secret")
	if e := s.next(t); strings.Contains(e[1], "secret") {
		t.Fatalf("event leaked build output: %v", e)
	}
}

func TestProxyBoundsOpenStreamsAndClosesThemOnClose(t *testing.T) {
	t.Parallel()
	f := newFixture(t, servePage(page))
	var streams []*stream
	for range maxClients {
		s := openStream(t, f.server.URL, "")
		s.next(t)
		streams = append(streams, s)
	}
	if over := openStream(t, f.server.URL, ""); over.status != http.StatusServiceUnavailable {
		t.Fatalf("stream %d status = %d, want 503", maxClients+1, over.status)
	}
	f.proxy.Close()
	for i, s := range streams {
		select {
		case <-s.done:
		case <-time.After(testWait):
			t.Fatalf("stream %d did not end on Close", i)
		}
	}
}

func TestProxyServeStopsWithContext(t *testing.T) {
	t.Parallel()
	proxy, err := NewProxy(ProxyOptions{Environment: "development"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- proxy.Serve(ctx, l) }()
	s := openStream(t, "http://"+l.Addr().String(), "")
	s.next(t)
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Serve did not stop with an open event stream")
	}
}

func TestLoopbackHost(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"localhost": true, "localhost:8080": true, "LOCALHOST": true, "127.0.0.1:3000": true, "[::1]:8080": true, "::1": true,
		"example.com": false, "192.168.1.5:8080": false, "localhost.evil.com": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestInjectScriptSearchesOnlyTheTail(t *testing.T) {
	t.Parallel()
	body := []byte("<html><body>" + strings.Repeat("a", 10_000) + "</body></html>")
	out, ok := injectScript(body, "n")
	if !ok || !bytes.HasSuffix(out, []byte(`nonce="n"></script></body></html>`)) {
		t.Fatalf("ok=%v tail=%q", ok, out[len(out)-60:])
	}
	if _, ok := injectScript([]byte("<body>"+strings.Repeat("a", 10_000)), "n"); ok {
		t.Fatal("a body without a closing tag must be left alone")
	}
}
