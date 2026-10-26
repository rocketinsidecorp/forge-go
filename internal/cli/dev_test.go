package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

func TestParseDevArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args    []string
		want    devOptions
		wantErr bool
	}{
		{args: nil, want: devOptions{pollInterval: 500 * time.Millisecond, debounce: 300 * time.Millisecond}},
		{args: []string{"api"}, want: devOptions{name: "api", pollInterval: 500 * time.Millisecond, debounce: 300 * time.Millisecond}},
		{args: []string{"--no-reload", "api"}, want: devOptions{name: "api", noReload: true, pollInterval: 500 * time.Millisecond, debounce: 300 * time.Millisecond}},
		{args: []string{"api", "--poll-interval", "1s", "--debounce=0s"}, want: devOptions{name: "api", pollInterval: time.Second}},
		{args: []string{"--poll-interval", "10ms"}, wantErr: true},
		{args: []string{"--poll-interval", "2h"}, wantErr: true},
		{args: []string{"--debounce", "-1s"}, wantErr: true},
		{args: []string{"a", "b"}, wantErr: true},
		{args: []string{"--no-reload", "a", "b"}, wantErr: true},
		{args: []string{"--unknown"}, wantErr: true},
	}
	for _, test := range tests {
		got, err := parseDevArgs(test.args, io.Discard)
		if (err != nil) != test.wantErr {
			t.Errorf("parseDevArgs(%v) error = %v, wantErr %v", test.args, err, test.wantErr)
			continue
		}
		if err == nil && got != test.want {
			t.Errorf("parseDevArgs(%v) = %+v, want %+v", test.args, got, test.want)
		}
	}
}

func TestLoopbackAddress(t *testing.T) {
	t.Parallel()
	for address, want := range map[string]bool{
		"127.0.0.1:8080": true, "localhost:8080": true, "[::1]:8080": true,
		":8080": false, "0.0.0.0:8080": false, "192.168.0.4:8080": false, "example.com:80": false, "garbage": false,
	} {
		if got := loopbackAddress(address); got != want {
			t.Errorf("loopbackAddress(%q) = %v, want %v", address, got, want)
		}
	}
}

func TestStartReloadProxyOnlyInDevelopmentOverPlainHTTP(t *testing.T) {
	t.Parallel()
	for name, env := range map[string][]string{
		"production":      {"FORGE_ENV=production", "FORGE_HTTP_ADDR=127.0.0.1:0", "FORGE_HTTP_TRANSPORT=trusted-proxy"},
		"staging":         {"FORGE_ENV=staging", "FORGE_HTTP_ADDR=127.0.0.1:0"},
		"test":            {"FORGE_ENV=test", "FORGE_HTTP_ADDR=127.0.0.1:0"},
		"trusted proxy":   {"FORGE_ENV=development", "FORGE_HTTP_ADDR=127.0.0.1:0", "FORGE_HTTP_TRANSPORT=trusted-proxy"},
		"invalid config":  {"FORGE_ENV=development", "FORGE_HTTP_ADDR=127.0.0.1:0", "FORGE_HTTP_READ_TIMEOUT=banana"},
		"unsupported env": {"FORGE_ENV=bogus"},
	} {
		var log bytes.Buffer
		proxy, listener, err := startReloadProxy(context.Background(), env, func(format string, args ...any) { log.WriteString(format + "\n") })
		if err != nil || proxy != nil || listener != nil {
			if listener != nil {
				_ = listener.Close()
			}
			t.Errorf("%s: proxy=%v listener=%v err=%v, want browser reload off", name, proxy, listener, err)
		}
		if !strings.Contains(log.String(), "browser reload is off") {
			t.Errorf("%s: the developer must be told why reload is off, log: %q", name, log.String())
		}
	}

	proxy, listener, err := startReloadProxy(context.Background(), []string{"FORGE_ENV=development", "FORGE_HTTP_ADDR=127.0.0.1:0"}, func(string, ...any) {})
	if err != nil || proxy == nil || listener == nil {
		t.Fatalf("development: proxy=%v listener=%v err=%v", proxy, listener, err)
	}
	_ = listener.Close()
}

func TestCappedBufferKeepsTheStartOfTheOutput(t *testing.T) {
	t.Parallel()
	var buffer cappedBuffer
	chunk := strings.Repeat("e", 1024)
	for range 100 {
		if n, err := buffer.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v; a short write would fail the compiler's output copy", n, err)
		}
	}
	got := buffer.String()
	if len(got) > buildOutputLimit+64 || !strings.HasSuffix(got, "output truncated") {
		t.Fatalf("buffer holds %d bytes, tail %q", len(got), got[len(got)-30:])
	}
}

const devApplication = `package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		fmt.Fprintf(w, "<html><body>%s greeting=%s host=%s</body></html>", VERSION, os.Getenv("GREETING"), r.Host)
	})
	server := &http.Server{Addr: os.Getenv("FORGE_HTTP_ADDR"), Handler: mux}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	go func() { <-stop; _ = server.Shutdown(context.Background()) }()
	_ = server.ListenAndServe()
}
`

func devSource(version string) string {
	return strings.Replace(devApplication, "VERSION", `"`+version+`"`, 1)
}

// lockedBuffer collects the dev runner's log from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestDevLoopRebuildsRestartsAndKeepsServingThroughABuildFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an application with the go tool")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/hello\n\ngo 1.21\n")
	mainFile := filepath.Join(root, "cmd", "hello", "main.go")
	writeFile(t, mainFile, devSource("v1"))

	address, err := freeLoopbackPort(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "FORGE_ENV=development", "FORGE_HTTP_ADDR="+address)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output lockedBuffer
	finished := make(chan error, 1)
	go func() {
		options := devOptions{pollInterval: 50 * time.Millisecond, debounce: 100 * time.Millisecond}
		finished <- devLoop(ctx, root, "hello", env, options, &output, &output)
	}()
	base := "http://" + address
	t.Cleanup(cancel)

	page := func() string {
		response, err := http.Get(base + "/") // #nosec G107 G704 -- loopback test server
		if err != nil {
			return ""
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode == http.StatusOK {
			return string(body)
		}
		return ""
	}
	waitFor := func(what string, condition func(string) bool) string {
		t.Helper()
		var body string
		if !waittest.Poll(2*time.Minute, func() bool {
			body = page()
			return body != "" && condition(body)
		}) {
			t.Fatalf("timed out waiting for %s\nforge dev output:\n%s", what, output.String())
		}
		return body
	}

	body := waitFor("the first build", func(b string) bool { return strings.Contains(b, "v1") })
	if !strings.Contains(body, "/__forge/reload.js") || !strings.Contains(body, "host="+address) {
		t.Fatalf("page must carry the reload script and the browser's Host: %s", body)
	}

	// Follow the event stream like a browser tab does.
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/__forge/events", nil)
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	events := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "data: ") {
				events <- line
			}
		}
	}()
	expectEvent := func(prefix string) string {
		t.Helper()
		deadline := time.After(2 * time.Minute)
		for {
			select {
			case line := <-events:
				if strings.HasPrefix(line, prefix) {
					return line
				}
			case <-deadline:
				t.Fatalf("no %q event\nforge dev output:\n%s", prefix, output.String())
			}
		}
	}
	expectEvent("event: hello")

	// A source change rebuilds, restarts and reloads the browser.
	writeFile(t, mainFile, devSource("v2"))
	expectEvent("event: reload")
	waitFor("v2", func(b string) bool { return strings.Contains(b, "v2") })

	// A broken build is reported and the running application keeps serving.
	writeFile(t, mainFile, strings.Replace(devSource("v3"), "http.NewServeMux()", "http.NewServeMux(", 1))
	expectEvent("event: build-error")
	if body := page(); !strings.Contains(body, "v2") {
		t.Fatalf("the previous build must keep serving, got %q", body)
	}
	if !strings.Contains(output.String(), "build failed") {
		t.Fatalf("the terminal must show the build failure:\n%s", output.String())
	}

	// The fix recovers.
	writeFile(t, mainFile, devSource("v3"))
	expectEvent("event: reload")
	waitFor("v3", func(b string) bool { return strings.Contains(b, "v3") })

	// An env file change restarts without rebuilding.
	writeFile(t, filepath.Join(root, ".env.local"), "GREETING=hello\n")
	expectEvent("event: reload")
	waitFor("the new environment", func(b string) bool { return strings.Contains(b, "greeting=hello") })
	if got := strings.Count(output.String(), "building"); got != 4 {
		t.Errorf("expected 4 builds (start, v2, broken, v3) and none for the env file, got %d\n%s", got, output.String())
	}

	// Interrupting stops the application and the proxy.
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("devLoop: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("devLoop did not stop")
	}
	if _, err := http.Get(base + "/"); err == nil { // #nosec G107 G704 -- loopback test server
		t.Fatal("the proxy must be closed after shutdown")
	}
}
