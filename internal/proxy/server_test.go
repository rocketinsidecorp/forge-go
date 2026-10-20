package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testHost = "app.example.com"

type backend struct {
	server  *httptest.Server
	healthy atomic.Bool
	hits    atomic.Int64
}

func (b *backend) addr() string { return strings.TrimPrefix(b.server.URL, "http://") }

func newBackend(t *testing.T, name string, handler http.HandlerFunc) *backend {
	t.Helper()
	b := &backend{}
	b.healthy.Store(true)
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == DefaultHealthPath {
			if b.healthy.Load() {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		b.hits.Add(1)
		if handler != nil {
			handler(w, r)
			return
		}
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(b.server.Close)
	return b
}

func newTestServer(t *testing.T, statePath string) (*Server, *httptest.Server) {
	t.Helper()
	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), statePath)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	return srv, front
}

func fastRequest(target string) DeployRequest {
	return DeployRequest{
		Service: "app", Target: target, Hosts: []string{testHost},
		HealthInterval: Duration(time.Millisecond), HealthyCount: 2,
		DeployTimeout: Duration(2 * time.Second), DrainTimeout: Duration(5 * time.Second),
	}
}

func get(t *testing.T, client *http.Client, front, host, path string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, front+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestSwitchUnderConcurrentLoadDropsNoRequests(t *testing.T) {
	slow := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
			_, _ = io.WriteString(w, name)
		}
	}
	a := newBackend(t, "a", slow("a"))
	b := newBackend(t, "b", slow("b"))
	srv, front := newTestServer(t, "")
	ctx := t.Context()
	if _, err := srv.Deploy(ctx, fastRequest(a.addr())); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}, Timeout: 10 * time.Second}
	var total, failures atomic.Int64
	var firstFailure atomic.Value
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				total.Add(1)
				req, _ := http.NewRequest(http.MethodPost, front.URL+"/work", strings.NewReader("payload"))
				req.Host = testHost
				resp, err := client.Do(req)
				if err != nil {
					failures.Add(1)
					firstFailure.CompareAndSwap(nil, err.Error())
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK || (string(body) != "a" && string(body) != "b") {
					failures.Add(1)
					firstFailure.CompareAndSwap(nil, resp.Status+" "+string(body))
				}
			}
		})
	}
	for i := range 20 {
		next := b
		if i%2 == 1 {
			next = a
		}
		result, err := srv.Deploy(ctx, fastRequest(next.addr()))
		if err != nil {
			t.Fatalf("deploy %d: %v", i, err)
		}
		if !result.Drained || result.InFlight != 0 {
			t.Fatalf("deploy %d not drained: %+v", i, result)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	workers.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d of %d requests failed during switches; first: %v", failures.Load(), total.Load(), firstFailure.Load())
	}
	if total.Load() < 100 || a.hits.Load() == 0 || b.hits.Load() == 0 {
		t.Fatalf("load too small to be meaningful: total=%d a=%d b=%d", total.Load(), a.hits.Load(), b.hits.Load())
	}
}

func TestUnhealthyTargetIsRejectedAndPreviousKeepsServing(t *testing.T) {
	old := newBackend(t, "old", nil)
	bad := newBackend(t, "bad", nil)
	bad.healthy.Store(false)
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(old.addr())); err != nil {
		t.Fatal(err)
	}
	req := fastRequest(bad.addr())
	req.DeployTimeout = Duration(100 * time.Millisecond)
	_, err := srv.Deploy(t.Context(), req)
	if !errors.Is(err, ErrUnhealthy) || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("Deploy error = %v, want ErrUnhealthy with last status", err)
	}
	if status, body, _ := get(t, http.DefaultClient, front.URL, testHost, "/"); status != 200 || body != "old" {
		t.Fatalf("after rejected deploy got %d %q, want old target", status, body)
	}
	if bad.hits.Load() != 0 {
		t.Fatal("unhealthy target received application traffic")
	}
	unreachable := fastRequest("127.0.0.1:1")
	unreachable.DeployTimeout = Duration(50 * time.Millisecond)
	if _, err := srv.Deploy(t.Context(), unreachable); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("unreachable target error = %v", err)
	}
}

func TestHealthGateRequiresConsecutivePasses(t *testing.T) {
	var probes atomic.Int64
	flapping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// pass, fail, pass, fail ... never two in a row
		if probes.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(flapping.Close)
	srv, _ := newTestServer(t, "")
	req := fastRequest(strings.TrimPrefix(flapping.URL, "http://"))
	req.DeployTimeout = Duration(150 * time.Millisecond)
	if _, err := srv.Deploy(t.Context(), req); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("flapping target accepted: %v", err)
	}
	if probes.Load() < 4 {
		t.Fatalf("only %d probes", probes.Load())
	}
	stable := newBackend(t, "stable", nil)
	req = fastRequest(stable.addr())
	req.HealthyCount = 3
	result, err := srv.Deploy(t.Context(), req)
	if err != nil || result.HealthChecks != 3 {
		t.Fatalf("stable deploy = %+v, %v; want 3 checks", result, err)
	}
}

func TestDeployDrainsInFlightRequestsBeforeReturning(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	old := newBackend(t, "old", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			_, _ = io.WriteString(w, "old")
			return
		}
		close(entered)
		<-release
		_, _ = io.WriteString(w, "old-finished")
	})
	next := newBackend(t, "new", nil)
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(old.addr())); err != nil {
		t.Fatal(err)
	}
	slowResult := make(chan string, 1)
	go func() {
		_, body, _ := get(t, http.DefaultClient, front.URL, testHost, "/slow")
		slowResult <- body
	}()
	<-entered
	deployed := make(chan DeployResult, 1)
	go func() {
		result, err := srv.Deploy(context.Background(), fastRequest(next.addr()))
		if err != nil {
			t.Error(err)
		}
		deployed <- result
	}()
	// New requests reach the new target while the old one drains.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, body, _ := get(t, http.DefaultClient, front.URL, testHost, "/"); body == "new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("traffic never switched")
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case result := <-deployed:
		t.Fatalf("deploy returned before the old request finished: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	result := <-deployed
	if !result.Drained || result.Previous != old.addr() {
		t.Fatalf("result = %+v", result)
	}
	if body := <-slowResult; body != "old-finished" {
		t.Fatalf("in-flight request got %q", body)
	}
}

func TestDrainTimeoutReportsRemainingRequests(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	old := newBackend(t, "old", func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(entered) })
		<-release
	})
	next := newBackend(t, "new", nil)
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(old.addr())); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		get(t, http.DefaultClient, front.URL, testHost, "/stuck")
	}()
	<-entered
	req := fastRequest(next.addr())
	req.DrainTimeout = Duration(30 * time.Millisecond)
	result, err := srv.Deploy(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Drained || result.InFlight != 1 {
		t.Fatalf("result = %+v, want undrained with one request", result)
	}
	close(release)
	<-done
}

func TestMaintenanceModeServesStaticPage(t *testing.T) {
	app := newBackend(t, "app", nil)
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetMaintenance("app", true); err != nil {
		t.Fatal(err)
	}
	status, body, header := get(t, http.DefaultClient, front.URL, testHost, "/")
	if status != http.StatusServiceUnavailable || header.Get("Retry-After") == "" || !strings.Contains(body, "maintenance") {
		t.Fatalf("maintenance response = %d %v %q", status, header, body)
	}
	if app.hits.Load() != 0 {
		t.Fatal("request reached the application during maintenance")
	}
	if err := srv.SetMaintenance("app", false); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := get(t, http.DefaultClient, front.URL, testHost, "/"); status != 200 {
		t.Fatalf("after maintenance off: %d", status)
	}
	if err := srv.SetMaintenance("missing", true); err == nil {
		t.Fatal("maintenance on unknown service accepted")
	}
}

func TestForwardedHeadersAreSetAndSpoofedOnesDropped(t *testing.T) {
	seen := make(chan http.Header, 1)
	hosts := make(chan string, 1)
	app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		hosts <- r.Host
	})
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/x", nil)
	req.Host = testHost
	req.Header.Set("X-Forwarded-For", "203.0.113.66")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Forwarded", "for=203.0.113.66;proto=https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	header := <-seen
	if got := header.Get("X-Forwarded-For"); got != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For = %q, want the connecting address only", got)
	}
	if got := header.Get("X-Forwarded-Proto"); got != "http" {
		t.Fatalf("X-Forwarded-Proto = %q, want http", got)
	}
	if got := header.Get("X-Forwarded-Host"); got != testHost {
		t.Fatalf("X-Forwarded-Host = %q", got)
	}
	if header.Get("Forwarded") != "" {
		t.Fatal("client Forwarded header reached the application")
	}
	if host := <-hosts; host != testHost {
		t.Fatalf("Host = %q, want original host", host)
	}
}

func TestRoutingByHost(t *testing.T) {
	one := newBackend(t, "one", nil)
	two := newBackend(t, "two", nil)
	srv, front := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(one.addr())); err != nil {
		t.Fatal(err)
	}
	second := fastRequest(two.addr())
	second.Service, second.Hosts = "other", []string{"other.example.com"}
	if _, err := srv.Deploy(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, http.DefaultClient, front.URL, "APP.example.com:80", "/"); body != "one" {
		t.Fatalf("app host routed to %q", body)
	}
	if _, body, _ := get(t, http.DefaultClient, front.URL, "other.example.com", "/"); body != "two" {
		t.Fatalf("other host routed to %q", body)
	}
	if status, _, _ := get(t, http.DefaultClient, front.URL, "unknown.example.com", "/"); status != http.StatusNotFound {
		t.Fatalf("unknown host status %d", status)
	}
	conflict := fastRequest(two.addr())
	conflict.Service = "third"
	if _, err := srv.Deploy(t.Context(), conflict); err == nil || !strings.Contains(err.Error(), "already routed") {
		t.Fatalf("host conflict accepted: %v", err)
	}
	if err := srv.Remove("other", time.Second); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := get(t, http.DefaultClient, front.URL, "other.example.com", "/"); status != http.StatusNotFound {
		t.Fatalf("removed service still routed: %d", status)
	}
}

func TestRequestBodyLimits(t *testing.T) {
	app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}
		_ = json.NewEncoder(w).Encode(n)
	})
	srv, front := newTestServer(t, "")
	req := fastRequest(app.addr())
	req.MaxBodyBytes = 1024
	req.BufferBytes = 512
	if _, err := srv.Deploy(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	post := func(body io.Reader, length int64) int {
		r, _ := http.NewRequest(http.MethodPost, front.URL+"/upload", body)
		r.Host = testHost
		r.ContentLength = length
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if status := post(bytes.NewReader(make([]byte, 100)), 100); status != 200 {
		t.Fatalf("buffered small body: %d", status)
	}
	if status := post(bytes.NewReader(make([]byte, 900)), 900); status != 200 {
		t.Fatalf("streamed body under limit: %d", status)
	}
	if status := post(bytes.NewReader(make([]byte, 2048)), 2048); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversize body: %d", status)
	}
	// Chunked body without Content-Length that exceeds the limit.
	if status := post(io.MultiReader(bytes.NewReader(make([]byte, 2048))), -1); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversize body: %d", status)
	}
}

func TestTLSServiceRedirectsPlainHTTP(t *testing.T) {
	app := newBackend(t, "app", nil)
	srv, front := newTestServer(t, "")
	req := fastRequest(app.addr())
	req.TLS = true
	if _, err := srv.Deploy(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	status, _, header := get(t, client, front.URL, testHost, "/a/b?c=d")
	if status != http.StatusPermanentRedirect || header.Get("Location") != "https://app.example.com/a/b?c=d" {
		t.Fatalf("redirect = %d %q", status, header.Get("Location"))
	}
	if !srv.TLSHost(testHost) || srv.TLSHost("other.example.com") {
		t.Fatal("TLSHost policy wrong")
	}
	if app.hits.Load() != 0 {
		t.Fatal("plain request reached the application")
	}
}

func TestInvalidDeployRequests(t *testing.T) {
	srv, _ := newTestServer(t, "")
	cases := map[string]DeployRequest{
		"bad service":  {Service: "Bad Name", Target: "a:80"},
		"no port":      {Service: "app", Target: "a"},
		"bad port":     {Service: "app", Target: "a:0"},
		"ip host":      {Service: "app", Target: "a:80", Hosts: []string{"10.0.0.1"}},
		"wildcard":     {Service: "app", Target: "a:80", Hosts: []string{"*.example.com"}},
		"dup host":     {Service: "app", Target: "a:80", Hosts: []string{"a.example.com", "A.example.com"}},
		"tls no hosts": {Service: "app", Target: "a:80", TLS: true},
		"health path":  {Service: "app", Target: "a:80", HealthPath: "health"},
		"count":        {Service: "app", Target: "a:80", HealthyCount: 1000},
		"timeout":      {Service: "app", Target: "a:80", DeployTimeout: Duration(-time.Second)},
	}
	for name, req := range cases {
		if _, err := srv.Deploy(t.Context(), req); err == nil || errors.Is(err, ErrUnhealthy) {
			t.Errorf("%s: error = %v, want validation error", name, err)
		}
	}
}

func TestControlSocketAndStatePersistence(t *testing.T) {
	dir, err := os.MkdirTemp("", "fp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	statePath := filepath.Join(dir, "state.json")
	socket := filepath.Join(dir, "run", "control.sock")
	app := newBackend(t, "app", nil)
	srv, _ := newTestServer(t, statePath)
	listener, err := ListenControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- srv.ServeControl(ctx, listener) }()
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v; want 0600", info, err)
	}
	if info, err := os.Stat(filepath.Dir(socket)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %v, %v", info, err)
	}

	request := fastRequest(app.addr())
	response, err := Call(ctx, socket, ControlRequest{Op: "deploy", Deploy: &request})
	if err != nil || !response.OK || response.Deploy.Target != app.addr() {
		t.Fatalf("deploy over socket = %+v, %v", response, err)
	}
	if response, err := Call(ctx, socket, ControlRequest{Op: "maintenance", Service: "app", Maintenance: true}); err != nil || !response.OK {
		t.Fatalf("maintenance = %+v, %v", response, err)
	}
	response, err = Call(ctx, socket, ControlRequest{Op: "status"})
	if err != nil || len(response.Services) != 1 || !response.Services[0].Maintenance || response.Services[0].Target != app.addr() {
		t.Fatalf("status = %+v, %v", response, err)
	}
	bad := DeployRequest{Service: "app", Target: "127.0.0.1:1", DeployTimeout: Duration(20 * time.Millisecond), HealthInterval: Duration(time.Millisecond)}
	response, err = Call(ctx, socket, ControlRequest{Op: "deploy", Deploy: &bad})
	if err != nil || response.OK || response.Code != "unhealthy" {
		t.Fatalf("unhealthy deploy over socket = %+v, %v", response, err)
	}
	if response, _ := Call(ctx, socket, ControlRequest{Op: "explode"}); response.OK {
		t.Fatal("unknown op accepted")
	}

	stateInfo, err := os.Stat(statePath)
	if err != nil || stateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, %v", stateInfo, err)
	}
	// A restarted proxy resumes routing from the saved state.
	restarted, front := newTestServer(t, statePath)
	statuses := restarted.Status()
	if len(statuses) != 1 || statuses[0].Target != app.addr() || !statuses[0].Maintenance {
		t.Fatalf("restored status = %+v", statuses)
	}
	if err := restarted.SetMaintenance("app", false); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, http.DefaultClient, front.URL, testHost, "/"); body != "app" {
		t.Fatalf("restored routing served %q", body)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	// A stale socket is replaced; a regular file is not.
	if listener, err := ListenControl(socket); err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	} else {
		_ = listener.Close()
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenControl(plain); err == nil {
		t.Fatal("regular file replaced by control socket")
	}
}

func TestAccessLogOmitsQuery(t *testing.T) {
	app := newBackend(t, "app", nil)
	var mu sync.Mutex
	var logs bytes.Buffer
	srv, err := New(slog.New(slog.NewJSONHandler(lockedWriter{&mu, &logs}, nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(srv)
	defer front.Close()
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	get(t, http.DefaultClient, front.URL, testHost, "/reset?token=s3cr3t")
	mu.Lock()
	defer mu.Unlock()
	var entry map[string]any
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, `"msg":"request"`) {
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatal(err)
			}
		}
	}
	if entry == nil || entry["path"] != "/reset" || entry["status"] != float64(200) || entry["service"] != "app" || entry["target"] != app.addr() {
		t.Fatalf("access log entry = %v", entry)
	}
	if strings.Contains(logs.String(), "s3cr3t") {
		t.Fatal("query string logged")
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
