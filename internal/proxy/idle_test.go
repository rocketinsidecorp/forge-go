package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// closedWithin reads from conn until the peer closes it; ok is false when
// it stays open for wait.
func closedWithin(conn net.Conn, wait time.Duration) (total int, ok bool) {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	buffer := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buffer)
		total += n
		if err != nil {
			var netErr net.Error
			return total, !errors.As(err, &netErr) || !netErr.Timeout()
		}
	}
}

func waitInFlight(t *testing.T, srv *Server, want int64, wait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		var got int64
		if statuses := srv.Status(); len(statuses) > 0 {
			got = statuses[0].InFlight
		}
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("in flight stayed %d, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func dialFront(t *testing.T, url string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A client that stops reading a response is disconnected once a write to
// it makes no progress for the idle timeout, which releases the target (a
// drain would otherwise wait for it).
func TestClientThatStopsReadingIsDisconnected(t *testing.T) {
	const size = 64 << 20
	app := newBackend(t, "app", func(w http.ResponseWriter, _ *http.Request) {
		// With a length the reverse proxy does not flush after each write:
		// the writes themselves must carry the deadline.
		w.Header().Set("Content-Length", strconv.Itoa(size))
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for written := 0; written < size; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	srv, front := newTestServer(t, "")
	srv.idleTimeout = 300 * time.Millisecond
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	conn := dialFront(t, front.URL)
	if _, err := io.WriteString(conn, "GET /large HTTP/1.1\r\nHost: "+testHost+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	waitInFlight(t, srv, 1, 5*time.Second)
	waitInFlight(t, srv, 0, 10*time.Second)
	received, closed := closedWithin(conn, 10*time.Second)
	if !closed {
		t.Fatal("connection to the stalled client kept open")
	}
	if received >= size {
		t.Fatalf("the whole body arrived (%d bytes)", received)
	}
}

// An upgraded connection stays open while data moves and is closed once it
// carried nothing in either direction for the idle timeout.
func TestIdleTunnelIsClosed(t *testing.T) {
	app := newBackend(t, "app", func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = rw.WriteString(line)
			_ = rw.Flush()
		}
	})
	srv, front := newTestServer(t, "")
	srv.idleTimeout = 400 * time.Millisecond
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	conn := dialFront(t, front.URL)
	_, err := io.WriteString(conn, "GET /cable HTTP/1.1\r\nHost: "+testHost+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	// Traffic every 100 ms for twice the idle timeout keeps it open.
	for round := range 8 {
		message := strings.Repeat("m", round+1) + "\n"
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.WriteString(conn, message); err != nil {
			t.Fatal(err)
		}
		got, err := reader.ReadString('\n')
		if err != nil || got != message {
			t.Fatalf("round %d: %q %v", round, got, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := srv.Status()[0].InFlight; got != 1 {
		t.Fatalf("in flight %d during the tunnel", got)
	}
	silent := time.Now()
	_ = conn.SetDeadline(time.Time{})
	if _, closed := closedWithin(conn, 5*time.Second); !closed {
		t.Fatal("idle tunnel kept open")
	}
	// The last echo was 100 ms before silent: about 300 ms remain.
	if elapsed := time.Since(silent); elapsed < 200*time.Millisecond {
		t.Fatalf("closed after %s, before the idle timeout", elapsed)
	}
	waitInFlight(t, srv, 0, 5*time.Second)
}

// A keep-alive connection waiting for its next request is closed after
// -idle-timeout.
func TestIdleKeepAliveConnectionIsClosed(t *testing.T) {
	app := newBackend(t, "app", nil)
	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := newHTTPServer(listener.Addr().String(), srv, nil, 10*time.Second, 300*time.Millisecond, DefaultMaxHeaderBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	conn := dialFront(t, "http://"+listener.Addr().String())
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: "+testHost+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response: %v %v", resp, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "app" {
		t.Fatalf("body %q", body)
	}
	started := time.Now()
	if _, closed := closedWithin(conn, 5*time.Second); !closed {
		t.Fatal("idle keep-alive connection kept open")
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("closed after %s", elapsed)
	}
}

func TestRunRefusesNonPositiveIdleTimeout(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		var stdout, stderr bytes.Buffer
		code := Main(t.Context(), []string{"run", "-idle-timeout", value, "-state", ""}, &stdout, &stderr)
		if code != ExitFailed || !strings.Contains(stderr.String(), "-idle-timeout must be positive") {
			t.Fatalf("%s: exit %d: %s", value, code, stderr.String())
		}
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func TestPrivateDirCreatesRestrictsAndRefusesLinks(t *testing.T) {
	root := t.TempDir()
	created := filepath.Join(root, "a", "b")
	if err := privateDir(created); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, created); got != 0o700 {
		t.Fatalf("created %o", got)
	}
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(open); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, open); got != 0o700 {
		t.Fatalf("existing %o", got)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(open, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(link); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("link: %v", err)
	}
	if got := mode(t, open); got != 0o755 {
		t.Fatalf("link target changed to %o", got)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(file); err == nil {
		t.Fatal("file accepted as a directory")
	}
}

func TestListenControlRestrictsAnExistingDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("", "fpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(run, 0o777); err != nil {
		t.Fatal(err)
	}
	listener, err := ListenControl(filepath.Join(run, "c.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	if got := mode(t, run); got != 0o700 {
		t.Fatalf("control directory %o", got)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(run, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenControl(filepath.Join(link, "c.sock")); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("link: %v", err)
	}
}

func TestRunRestrictsAnExistingACMECache(t *testing.T) {
	dir, err := os.MkdirTemp("", "fpa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cache := filepath.Join(dir, "acme")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"run", "-http-addr", "127.0.0.1:0", "-https-addr", "127.0.0.1:0",
			"-control", filepath.Join(dir, "c.sock"), "-state", "", "-acme-cache", cache}, &stdout, &stderr)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if response, err := Call(ctx, filepath.Join(dir, "c.sock"), ControlRequest{Op: "status"}); err == nil && response.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not start: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := mode(t, cache); got != 0o700 {
		t.Fatalf("ACME cache %o", got)
	}
	cancel()
	if code := <-done; code != ExitOK {
		t.Fatalf("run exit %d: %s", code, stderr.String())
	}
}

func TestDurableCacheStoresPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	cache := durableCache{autocert.DirCache(dir)}
	if err := cache.Put(t.Context(), "app.example.com", []byte("pem")); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(t.Context(), "app.example.com")
	if err != nil || string(got) != "pem" {
		t.Fatalf("get: %q %v", got, err)
	}
	if got := mode(t, filepath.Join(dir, "app.example.com")); got != 0o600 {
		t.Fatalf("cache file %o", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left: %v %v", entries, err)
	}
	// A cancelled Put leaves the entry as it was.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cache.Put(ctx, "app.example.com", []byte("new")); err == nil {
		t.Fatal("cancelled Put succeeded")
	}
	if got, _ := cache.Get(t.Context(), "app.example.com"); string(got) != "pem" {
		t.Fatalf("cancelled Put overwrote the entry: %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left after a cancelled Put: %v", entries)
	}
}

func TestPrivateDirRefusesADirectoryOfAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give a directory to another user")
	}
	dir := filepath.Join(t.TempDir(), "foreign")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 54321, 54321); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(dir); err == nil || !strings.Contains(err.Error(), "is owned by uid 54321") {
		t.Fatalf("foreign directory: %v", err)
	}
}

// The write deadline of one request does not carry over to the next request
// on the same keep-alive connection (net/http clears it after each
// response): a slow response without a body, sent after the previous
// deadline passed, still arrives.
func TestWriteDeadlineDoesNotCarryOverToTheNextRequest(t *testing.T) {
	app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(400 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "app")
	})
	srv, front := newTestServer(t, "")
	srv.idleTimeout = 300 * time.Millisecond
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	conn := dialFront(t, front.URL)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	for _, step := range []struct {
		path   string
		status int
	}{{"/", http.StatusOK}, {"/slow", http.StatusNoContent}} {
		if _, err := io.WriteString(conn, "GET "+step.path+" HTTP/1.1\r\nHost: "+testHost+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("%s: %v", step.path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != step.status {
			t.Fatalf("%s: status %d", step.path, resp.StatusCode)
		}
		// Let the first request's deadline pass before the next one.
		time.Sleep(500 * time.Millisecond)
	}
}
