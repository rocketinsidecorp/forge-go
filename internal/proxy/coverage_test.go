package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortDir returns a temporary directory with a path short enough for a Unix
// socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fpc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// recordingHandler keeps every log record and announces the access log lines.
type recordingHandler struct {
	mu       sync.Mutex
	records  []slog.Record
	requests chan slog.Record
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{requests: make(chan slog.Record, 16)}
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	if r.Message == "request" {
		h.requests <- r
	}
	return nil
}

func (h *recordingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		out = append(out, r.Message)
	}
	return out
}

func attrOf(r slog.Record, key string) slog.Value {
	var found slog.Value
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = a.Value
			return false
		}
		return true
	})
	return found
}

func newLoggedServer(t *testing.T) (*Server, *httptest.Server, *recordingHandler) {
	t.Helper()
	handler := newRecordingHandler()
	srv, err := New(slog.New(handler), "")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	return srv, front, handler
}

func nextRequest(t *testing.T, h *recordingHandler) slog.Record {
	t.Helper()
	select {
	case r := <-h.requests:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no access log line")
		return slog.Record{}
	}
}

// --- config ---

func TestDurationJSON(t *testing.T) {
	t.Parallel()
	var d Duration
	if err := json.Unmarshal([]byte(`"1m30s"`), &d); err != nil || time.Duration(d) != 90*time.Second {
		t.Fatalf("1m30s = %v, %v", d, err)
	}
	if encoded, err := json.Marshal(d); err != nil || string(encoded) != `"1m30s"` {
		t.Fatalf("encoded %s, %v", encoded, err)
	}
	for in, want := range map[string]string{
		`5`:         `duration must be a string such as "5s"`,
		`null5`:     `duration must be a string`,
		`"soon"`:    `invalid duration "soon"`,
		`["1s"]`:    `duration must be a string`,
		`{"a":"1"}`: `duration must be a string`,
	} {
		var got Duration
		if err := got.UnmarshalJSON([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", in, err, want)
		}
	}
}

func TestDeployRequestValidation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*DeployRequest)
		want   string
	}{
		"target host starting with a dash": {func(r *DeployRequest) { r.Target = "-bad:80" }, "target host is invalid"},
		"target host with a space":         {func(r *DeployRequest) { r.Target = "bad host:80" }, "target host is invalid"},
		"target without a port":            {func(r *DeployRequest) { r.Target = "backend" }, "target must be host:port"},
		"target port out of range":         {func(r *DeployRequest) { r.Target = "backend:70000" }, "port must be 1-65535"},
		"negative body limit":              {func(r *DeployRequest) { r.MaxBodyBytes = -1 }, "body limits must be positive"},
		"negative buffer":                  {func(r *DeployRequest) { r.BufferBytes = -1 }, "body limits must be positive"},
	} {
		req := DeployRequest{Service: "app", Target: "backend:80"}
		tc.mutate(&req)
		if err := req.normalize(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// The buffer never exceeds the body limit.
	req := DeployRequest{Service: "app", Target: "backend:80", MaxBodyBytes: 100, BufferBytes: 1000}
	if err := req.normalize(); err != nil || req.BufferBytes != 100 {
		t.Errorf("buffer = %d, err %v", req.BufferBytes, err)
	}
}

// --- control socket ---

func TestListenControlReplacesAStaleSocketAndRefusesBadPaths(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	path := filepath.Join(dir, "c.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Closing a listener normally removes its socket; keep the file, as a
	// crashed process would.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the stale socket is gone: %v", err)
	}
	listener, err := ListenControl(path)
	if err != nil {
		t.Fatalf("a stale socket must be replaced: %v", err)
	}
	_ = listener.Close()

	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenControl(filepath.Join(notADir, "c.sock")); err == nil || !strings.Contains(err.Error(), "control directory: create") {
		t.Errorf("a directory that cannot be created: %v", err)
	}
	tooLong := filepath.Join(dir, strings.Repeat("d", 120), "c.sock")
	if _, err := ListenControl(tooLong); err == nil || !strings.Contains(err.Error(), "listen on control socket") {
		t.Errorf("a path longer than a socket address allows: %v", err)
	}
}

type failingListener struct {
	net.Listener
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }

func TestServeControlReturnsAnAcceptFailure(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "")
	boom := errors.New("accept failed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inner.Close() }()
	if err := srv.ServeControl(ctx, failingListener{Listener: inner, err: boom}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func serveControlOn(t *testing.T, srv *Server) (string, context.Context) {
	t.Helper()
	socket := filepath.Join(shortDir(t), "c.sock")
	listener, err := ListenControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.ServeControl(ctx, listener); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return socket, ctx
}

func TestControlSocketRejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "")
	socket, _ := serveControlOn(t, srv)
	for name, payload := range map[string]string{
		"not json":      "this is not json\n",
		"unknown field": `{"op":"status","bogus":1}` + "\n",
		"wrong type":    `{"op":7}` + "\n",
		"empty":         "",
	} {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, payload)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		var response ControlResponse
		err = json.NewDecoder(conn).Decode(&response)
		_ = conn.Close()
		if err != nil || response.OK || response.Error != "invalid control request" {
			t.Errorf("%s: response %+v, err %v", name, response, err)
		}
	}
}

func TestControlOperationsAnswerErrorsAsData(t *testing.T) {
	t.Parallel()
	app := newBackend(t, "app", nil)
	srv, _ := newTestServer(t, "")
	socket, ctx := serveControlOn(t, srv)
	call := func(request ControlRequest) ControlResponse {
		t.Helper()
		response, err := Call(ctx, socket, request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	if r := call(ControlRequest{Op: "deploy"}); r.OK || r.Error != "deploy request missing" {
		t.Errorf("deploy without a body: %+v", r)
	}
	invalid := DeployRequest{Service: "Not Valid", Target: app.addr()}
	if r := call(ControlRequest{Op: "deploy", Deploy: &invalid}); r.OK || r.Code != "invalid" {
		t.Errorf("invalid deploy: %+v", r)
	}
	if r := call(ControlRequest{Op: "remove", Service: "ghost"}); r.OK || !strings.Contains(r.Error, `unknown service "ghost"`) {
		t.Errorf("remove of an unknown service: %+v", r)
	}
	if r := call(ControlRequest{Op: "maintenance", Service: "ghost", Maintenance: true}); r.OK || !strings.Contains(r.Error, `unknown service "ghost"`) {
		t.Errorf("maintenance of an unknown service: %+v", r)
	}
	request := fastRequest(app.addr())
	if r := call(ControlRequest{Op: "deploy", Deploy: &request}); !r.OK {
		t.Fatalf("deploy: %+v", r)
	}
	// Without a drain timeout the default applies; nothing is in flight, so
	// the removal returns at once.
	if r := call(ControlRequest{Op: "remove", Service: "app"}); !r.OK {
		t.Errorf("remove: %+v", r)
	}
	if r := call(ControlRequest{Op: "status"}); !r.OK || len(r.Services) != 0 {
		t.Errorf("status after remove: %+v", r)
	}
}

func TestCallReportsBrokenExchanges(t *testing.T) {
	t.Parallel()
	t.Run("the server hangs up", func(t *testing.T) {
		t.Parallel()
		socket := filepath.Join(shortDir(t), "c.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				_ = conn.Close()
			}
		}()
		_, err = Call(context.Background(), socket, ControlRequest{Op: "status"})
		if err == nil || !strings.Contains(err.Error(), "control") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the server never answers and the context ends", func(t *testing.T) {
		t.Parallel()
		socket := filepath.Join(shortDir(t), "c.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		accepted := make(chan net.Conn, 1)
		requested := make(chan struct{})
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Read the request line: the client has sent its request and now
			// waits for a response that never comes.
			_, _ = bufio.NewReader(conn).ReadString('\n')
			close(requested)
			accepted <- conn
		}()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := Call(ctx, socket, ControlRequest{Op: "status"})
			result <- err
		}()
		<-requested
		conn := <-accepted
		defer func() { _ = conn.Close() }()
		cancel() // the request was sent; the response never comes
		if err := <-result; err == nil || !strings.Contains(err.Error(), "read control response") {
			t.Fatalf("err = %v", err)
		}
	})
}

// --- command line ---

func TestMainClientCommandsReportFailures(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "")
	socket, ctx := serveControlOn(t, srv)
	app := newBackend(t, "app", nil)
	request := fastRequest(app.addr())
	if r, err := Call(ctx, socket, ControlRequest{Op: "deploy", Deploy: &request}); err != nil || !r.OK {
		t.Fatalf("deploy: %+v %v", r, err)
	}
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := Main(ctx, args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	for name, tc := range map[string]struct {
		args   []string
		code   int
		stderr string
	}{
		"unknown flag":             {[]string{"status", "-bogus"}, ExitUsage, "flag provided but not defined"},
		"unexpected argument":      {[]string{"status", "-control", socket, "extra"}, ExitUsage, "unexpected arguments"},
		"maintenance needs on/off": {[]string{"maintenance", "-control", socket, "-service", "app"}, ExitUsage, "usage: forge-proxy maintenance"},
		"maintenance bad word":     {[]string{"maintenance", "-control", socket, "-service", "app", "maybe"}, ExitUsage, "usage: forge-proxy maintenance"},
		"maintenance unknown":      {[]string{"maintenance", "-control", socket, "-service", "ghost", "on"}, ExitFailed, `unknown service "ghost"`},
		"remove unknown":           {[]string{"remove", "-control", socket, "-service", "ghost"}, ExitFailed, `unknown service "ghost"`},
		"deploy invalid":           {[]string{"deploy", "-control", socket, "-service", "Bad Name", "-target", app.addr()}, ExitFailed, "service name must match"},
		"unreachable control":      {[]string{"status", "-control", filepath.Join(shortDir(t), "none.sock")}, ExitFailed, "connect to forge-proxy control socket"},
	} {
		if code, _, stderr := run(tc.args...); code != tc.code || !strings.Contains(stderr, tc.stderr) {
			t.Errorf("%s: exit %d (want %d), stderr %q (want %q)", name, code, tc.code, stderr, tc.stderr)
		}
	}
	code, out, _ := run("remove", "-control", socket, "-service", "app", "-drain-timeout", "1s")
	if code != ExitOK || !strings.Contains(out, `"ok":true`) {
		t.Errorf("remove: exit %d, %s", code, out)
	}
	if code, _, stderr := run("healthcheck", "-control", socket, "stray"); code != ExitUsage || !strings.Contains(stderr, "unexpected arguments") {
		t.Errorf("healthcheck with an argument: exit %d, %s", code, stderr)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func TestRunRejectsBadStartups(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown flag":       {[]string{"run", "-bogus"}, "flag provided but not defined"},
		"positional":         {[]string{"run", "extra"}, "run accepts flags only"},
		"corrupt state":      {[]string{"run", "-state", corrupt, "-control", filepath.Join(dir, "a.sock")}, "decode proxy state"},
		"control path":       {[]string{"run", "-state", "", "-control", filepath.Join(file, "a.sock")}, "control directory: create"},
		"http address taken": {[]string{"run", "-state", "", "-https-addr", "", "-http-addr", taken.Addr().String(), "-control", filepath.Join(dir, "b.sock")}, "address already in use"},
		"https address":      {[]string{"run", "-state", "", "-https-addr", taken.Addr().String(), "-http-addr", "127.0.0.1:0", "-control", filepath.Join(dir, "c.sock"), "-acme-cache", dir}, "address already in use"},
	} {
		var stdout, stderr bytes.Buffer
		code := Main(context.Background(), tc.args, &stdout, &stderr)
		if code != ExitFailed || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: exit %d, stderr %q, want %q", name, code, stderr.String(), tc.want)
		}
	}
}

// TestRunAppliesTheTLSHostPolicyAndFailsALateShutdown starts the real
// listeners, asks for certificates through TLS handshakes (the host policy
// answers before any ACME request: an ACME directory that refuses connections
// proves nothing leaves the process) and then stops with a shutdown budget
// that an active request outlasts.
func TestRunAppliesTheTLSHostPolicyAndFailsALateShutdown(t *testing.T) {
	dir := shortDir(t)
	socket := filepath.Join(dir, "c.sock")
	httpAddr, httpsAddr := freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"run", "-http-addr", httpAddr, "-https-addr", httpsAddr, "-control", socket,
			"-state", "", "-acme-cache", filepath.Join(dir, "acme"), "-acme-email", "ops@example.com",
			"-acme-directory", "http://" + freeAddr(t) + "/directory", "-shutdown-timeout", "1ms"}, &stdout, &stderr)
	}()
	waitForControl(t, ctx, socket)

	hold, release := make(chan struct{}), make(chan struct{})
	app := newBackend(t, "app", func(w http.ResponseWriter, _ *http.Request) {
		close(hold)
		<-release
	})
	tlsRequest := fastRequest(app.addr())
	tlsRequest.TLS = true
	response, err := Call(ctx, socket, ControlRequest{Op: "deploy", Deploy: &tlsRequest})
	if err != nil || !response.OK {
		t.Fatalf("deploy: %+v %v", response, err)
	}

	handshake := func(serverName string) error {
		conn, err := tls.Dial("tcp", httpsAddr, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}) // #nosec G402 -- the handshake is expected to fail before any certificate
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	if err := handshake("unknown.example.com"); err == nil {
		t.Error("a certificate was issued for a host no service has")
	}
	if err := handshake(testHost); err == nil {
		t.Error("a certificate was issued without an ACME server")
	}

	// An active request holds the plain listener open past the shutdown budget.
	conn, err := net.Dial("tcp", httpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Plain HTTP to a TLS service redirects; use a Host that forces forwarding
	// instead: the second service answers without TLS.
	other := fastRequest(app.addr())
	other.Service, other.Hosts = "plain", []string{"plain.example.com"}
	if r, err := Call(ctx, socket, ControlRequest{Op: "deploy", Deploy: &other}); err != nil || !r.OK {
		t.Fatalf("deploy plain: %+v %v", r, err)
	}
	_, _ = io.WriteString(conn, "GET /slow HTTP/1.1\r\nHost: plain.example.com\r\n\r\n")
	<-hold // the application holds the request: the connection is active
	cancel()
	select {
	case code := <-done:
		if code != ExitFailed || !strings.Contains(stderr.String(), "deadline exceeded") {
			t.Errorf("exit %d, stderr %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	close(release)
	if !strings.Contains(stdout.String(), "forge-proxy stopped") {
		t.Errorf("the stop was not logged: %s", stdout.String())
	}
}

// syncBuffer is a bytes.Buffer that a server goroutine may write while the
// test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForControl(t *testing.T, ctx context.Context, socket string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if response, err := Call(ctx, socket, ControlRequest{Op: "status"}); err == nil && response.OK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the proxy did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- forwarding ---

func TestUpstreamFailuresAnswerBadGateway(t *testing.T) {
	t.Parallel()
	hangUp := newBackend(t, "app", func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	srv, front, logs := newLoggedServer(t)
	if _, err := srv.Deploy(t.Context(), fastRequest(hangUp.addr())); err != nil {
		t.Fatal(err)
	}
	status, body, _ := get(t, http.DefaultClient, front.URL, testHost, "/")
	if status != http.StatusBadGateway || !strings.Contains(body, "bad gateway") {
		t.Fatalf("status %d body %q", status, body)
	}
	if got := attrOf(nextRequest(t, logs), "status").Int64(); got != http.StatusBadGateway {
		t.Errorf("access log status %d", got)
	}
	if !slicesContain(logs.messages(), "upstream error") {
		t.Errorf("the upstream error was not logged: %v", logs.messages())
	}
}

func slicesContain(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestClientDisconnectIsLoggedAs499(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	app := newBackend(t, "app", func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	srv, front, logs := newLoggedServer(t)
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/slow", nil)
	request.Host = testHost
	errc := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("the cancelled request succeeded")
	}
	entry := nextRequest(t, logs)
	if attrOf(entry, "status").Int64() != 499 || attrOf(entry, "service").String() != "app" {
		t.Errorf("access log: status %v service %v", attrOf(entry, "status"), attrOf(entry, "service"))
	}
	for _, message := range logs.messages() {
		if message == "upstream error" {
			t.Error("a client that went away is not an upstream error")
		}
	}
}

func TestChunkedBodyBeyondTheLimitIsRejectedWhileBuffering(t *testing.T) {
	t.Parallel()
	app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	srv, front := newTestServer(t, "")
	req := fastRequest(app.addr())
	req.MaxBodyBytes, req.BufferBytes = 10, 10 // everything within the limit is buffered
	if _, err := srv.Deploy(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	post := func(size int) int {
		request, _ := http.NewRequest(http.MethodPost, front.URL+"/upload", io.NopCloser(bytes.NewReader(make([]byte, size))))
		request.Host = testHost // an opaque reader: the body is sent chunked
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	if status := post(8); status != http.StatusOK {
		t.Errorf("a chunked body within the limit: %d", status)
	}
	if status := post(20); status != http.StatusRequestEntityTooLarge {
		t.Errorf("a chunked body beyond the limit: %d", status)
	}
}

// stalledUpload announces contentLength bytes, sends sent of them and stalls.
// It returns the status line, how long the answer took and whether the
// proxy then closed the connection.
func stalledUpload(t *testing.T, front string, contentLength, sent int) (status string, took time.Duration, closed bool) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(front, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	start := time.Now()
	_, _ = fmt.Fprintf(conn, "POST /upload HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n%s", testHost, contentLength, strings.Repeat("x", sent))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	status, err = reader.ReadString('\n')
	if err != nil {
		t.Fatalf("no answer after %s: %v", time.Since(start), err)
	}
	took = time.Since(start)
	// Drain the rest of the answer: the connection must then end.
	_, err = io.Copy(io.Discard, reader)
	closed = err == nil
	return status, took, closed
}

func TestStalledBodyWithADeclaredLengthGets408PromptlyAndTheConnectionCloses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		bufferBytes int64
		length      int
	}{
		// Above the buffer the body streams; at or below it is read before
		// the application is contacted.
		"streamed": {16, 5000},
		"buffered": {1024, 500},
		"at limit": {500, 500},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
			srv, front := newTestServer(t, "")
			srv.bodyTimeout = time.Second
			req := fastRequest(app.addr())
			req.BufferBytes = tc.bufferBytes
			if _, err := srv.Deploy(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			status, took, closed := stalledUpload(t, front.URL, tc.length, 10)
			if !strings.HasPrefix(status, "HTTP/1.1 408") {
				t.Fatalf("status line %q", status)
			}
			// One body timeout plus slack for a loaded runner; a drain with a
			// fresh timeout would take two (2s), one with none at all would
			// never end.
			if took > 1700*time.Millisecond {
				t.Errorf("the 408 took %s with a 1s body timeout", took)
			}
			if !closed {
				t.Error("the connection stayed open after the 408")
			}
			if tc.bufferBytes >= int64(tc.length) && app.hits.Load() != 0 {
				t.Errorf("the application was contacted %d times for a body that never arrived", app.hits.Load())
			}
		})
	}
}

func TestAccessLogUsesTheRemoteAddressWithoutAPort(t *testing.T) {
	t.Parallel()
	srv, _, logs := newLoggedServer(t)
	request := httptest.NewRequest(http.MethodGet, "http://nowhere.example/", nil)
	request.RemoteAddr = "pipe"
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)
	entry := nextRequest(t, logs)
	if recorder.Code != http.StatusNotFound || attrOf(entry, "client_ip").String() != "pipe" || attrOf(entry, "status").Int64() != 404 {
		t.Errorf("status %d, client_ip %q", recorder.Code, attrOf(entry, "client_ip").String())
	}
	// A handler that writes nothing is logged as the 200 net/http sends.
	if got := (&statusRecorder{ResponseWriter: httptest.NewRecorder()}); got.status != 0 {
		t.Fatalf("a fresh recorder has status %d", got.status)
	}
}

func TestStatusRecorderDefaultsToOKAndForwardsFlush(t *testing.T) {
	t.Parallel()
	inner := httptest.NewRecorder()
	recorder := &statusRecorder{ResponseWriter: inner, controller: http.NewResponseController(inner), idle: time.Second}
	recorder.Flush()
	if !inner.Flushed {
		t.Error("Flush did not reach the response writer")
	}
	if _, err := recorder.Write([]byte("abc")); err != nil || recorder.status != http.StatusOK || recorder.bytes != 3 {
		t.Errorf("status %d bytes %d err %v", recorder.status, recorder.bytes, err)
	}
	// A writer that cannot flush is not an error.
	plain := noFlushWriter{http.Header{}}
	(&statusRecorder{ResponseWriter: plain, controller: http.NewResponseController(plain), idle: time.Second}).Flush()
}

type noFlushWriter struct{ header http.Header }

func (w noFlushWriter) Header() http.Header       { return w.header }
func (noFlushWriter) Write(p []byte) (int, error) { return len(p), nil }
func (noFlushWriter) WriteHeader(int)             {}

// --- deploys ---

func TestHostsAreExclusiveAndOnlyOneServiceTakesUnmatchedHosts(t *testing.T) {
	t.Parallel()
	app := newBackend(t, "app", nil)
	srv, _ := newTestServer(t, "")
	req := fastRequest(app.addr())
	req.Hosts = nil
	if _, err := srv.Deploy(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	other := fastRequest(app.addr())
	other.Service, other.Hosts = "second", nil
	if _, err := srv.Deploy(t.Context(), other); err == nil || !strings.Contains(err.Error(), "service app already receives requests for unmatched hosts") {
		t.Fatalf("a second fallback service: %v", err)
	}
	other.Hosts = []string{"second.example.com"}
	if _, err := srv.Deploy(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	// The listing is sorted by name.
	var names []string
	for _, status := range srv.Status() {
		names = append(names, status.Service)
	}
	if strings.Join(names, ",") != "app,second" {
		t.Errorf("status order %v", names)
	}
	// Unmatched hosts reach the service without hosts.
	if svc := srv.route("anything.example.org"); svc == nil || svc.name != "app" {
		t.Errorf("fallback route = %v", svc)
	}
}

func TestAHostClaimedDuringTheHealthCheckRejectsTheLateDeploy(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "")
	probing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(probing) })
		<-release
	}))
	t.Cleanup(slow.Close)
	fast := newBackend(t, "fast", nil)

	first := fastRequest(strings.TrimPrefix(slow.URL, "http://"))
	first.Service, first.HealthyCount = "first", 1
	result := make(chan error, 1)
	go func() {
		_, err := srv.Deploy(t.Context(), first)
		result <- err
	}()
	<-probing // the first deploy passed the host check and waits for its health check
	second := fastRequest(fast.addr())
	second.Service = "second"
	if _, err := srv.Deploy(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "host "+testHost+" is already routed to service second") {
		t.Fatalf("the late deploy: %v", err)
	}
	for _, status := range srv.Status() {
		if status.Service == "first" {
			t.Error("the rejected deploy left a service behind")
		}
	}
}

func TestHealthPathThatCannotBeRequestedIsUnhealthy(t *testing.T) {
	t.Parallel()
	app := newBackend(t, "app", nil)
	srv, _ := newTestServer(t, "")
	req := fastRequest(app.addr())
	req.HealthPath = "/%zz"
	req.DeployTimeout = Duration(50 * time.Millisecond)
	_, err := srv.Deploy(t.Context(), req)
	if !errors.Is(err, ErrUnhealthy) || !strings.Contains(err.Error(), "invalid URL escape") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoveOfAnUnknownServiceFails(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "")
	if err := srv.Remove("ghost", time.Second); err == nil || !strings.Contains(err.Error(), `unknown service "ghost"`) {
		t.Fatalf("err = %v", err)
	}
}

// --- state ---

func TestLoadStateRejectsWhatItCannotTrust(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	service := func(name, host string) string {
		return fmt.Sprintf(`{"settings":{"service":%q,"target":"127.0.0.1:9","hosts":[%q]}}`, name, host)
	}
	asDirectory := filepath.Join(dir, "is-a-directory")
	if err := os.Mkdir(asDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"unreadable":       {asDirectory, "read proxy state"},
		"not json":         {write("a.json", "{"), "decode proxy state"},
		"unknown version":  {write("b.json", `{"version":99,"services":[]}`), "unsupported version 99"},
		"invalid settings": {write("c.json", `{"version":1,"services":[{"settings":{"service":"BAD","target":"127.0.0.1:9"}}]}`), `proxy state service "BAD"`},
		"host conflict": {write("d.json", `{"version":1,"services":[`+service("one", "x.example.com")+","+service("two", "x.example.com")+`]}`),
			"already routed to service"},
	} {
		srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), tc.path)
		if err == nil || srv != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// A missing file is a fresh proxy.
	if srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), filepath.Join(dir, "missing.json")); err != nil || srv == nil {
		t.Errorf("a missing state file: %v", err)
	}
}

func TestSaveStateFailuresAreReportedNotFatal(t *testing.T) {
	t.Parallel()
	app := newBackend(t, "app", nil)
	dir := shortDir(t)

	// The directory does not exist: the temporary file cannot be created.
	srv, _ := newTestServer(t, filepath.Join(dir, "missing", "state.json"))
	result, err := srv.Deploy(t.Context(), fastRequest(app.addr()))
	if err != nil || !strings.Contains(result.StateSaveFailed, "write proxy state") {
		t.Fatalf("deploy: %+v, %v", result, err)
	}
	if status := srv.Status(); len(status) != 1 || status[0].Target != app.addr() {
		t.Errorf("the deploy must still take effect: %+v", status)
	}
	if err := srv.SetMaintenance("app", true); err == nil {
		t.Error("SetMaintenance must report that the state was not saved")
	}

	// The state path is a directory: the rename over it fails and no
	// temporary file is left behind.
	asDirectory := filepath.Join(dir, "state-dir")
	if err := os.Mkdir(asDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	srv, _ = newTestServer(t, "")
	srv.statePath = asDirectory
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	if err := srv.saveState(); err == nil || !strings.Contains(err.Error(), "write proxy state") {
		t.Errorf("saveState: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state-") {
			t.Errorf("temporary file %s left behind", entry.Name())
		}
	}
}

// deadlineRecorder is a ResponseWriter that records the read deadlines set
// through http.ResponseController.
type deadlineRecorder struct {
	http.ResponseWriter
	mu    sync.Mutex
	calls []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, t)
	return nil
}

func (d *deadlineRecorder) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// Once the handler has returned the connection belongs to the server's next
// read: a body the transport still reads or closes must not set or clear its
// deadline.
func TestStreamedBodyIsInertOnceTheHandlerReturned(t *testing.T) {
	t.Parallel()
	writer := &deadlineRecorder{ResponseWriter: httptest.NewRecorder()}
	recorder := &statusRecorder{ResponseWriter: writer, controller: http.NewResponseController(writer), idle: time.Second}
	srv, _ := newTestServer(t, "")
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 100)))
	request.ContentLength = 100
	if !srv.prepareBody(recorder, request, 10) {
		t.Fatal("prepareBody refused a streamed body")
	}
	body, ok := request.Body.(*deadlineBody)
	if !ok {
		t.Fatalf("body is %T", request.Body)
	}
	buffer := make([]byte, 10)
	if _, err := body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if writer.count() != 1 {
		t.Fatalf("a read during the request sets its deadline: %d calls", writer.count())
	}
	recorder.finished.Store(true)
	if _, err := body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if writer.count() != 1 {
		t.Errorf("a body read or closed after the handler returned touched the deadline: %d calls", writer.count())
	}
}
