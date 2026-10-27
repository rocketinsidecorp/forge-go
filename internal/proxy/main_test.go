package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

func TestMainClientCommands(t *testing.T) {
	dir, err := os.MkdirTemp("", "fpm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "c.sock")
	srv, front := newTestServer(t, "")
	listener, err := ListenControl(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = srv.ServeControl(ctx, listener) }()
	app := newBackend(t, "app", nil)

	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := Main(ctx, append(args, "-control", socket), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	// flags must precede positional arguments for the flag package, so
	// maintenance is invoked with the socket flag first.
	code, out, errOut := run("deploy", "-service", "app", "-target", app.addr(), "-hosts", testHost, "-health-interval", "1ms", "-healthy-count", "1")
	if code != ExitOK {
		t.Fatalf("deploy exit %d: %s %s", code, out, errOut)
	}
	var response ControlResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil || response.Deploy.Target != app.addr() {
		t.Fatalf("deploy output %q: %v", out, err)
	}
	if code, _, _ := run("healthcheck"); code != ExitOK {
		t.Fatalf("healthcheck exit %d", code)
	}
	code, _, errOut = run("deploy", "-service", "app", "-target", "127.0.0.1:1", "-deploy-timeout", "20ms", "-health-interval", "1ms")
	if code != ExitUnhealthy || !strings.Contains(errOut, "not healthy") {
		t.Fatalf("unhealthy deploy exit %d: %s", code, errOut)
	}
	var stdout, stderr bytes.Buffer
	if code := Main(ctx, []string{"maintenance", "-control", socket, "-service", "app", "on"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("maintenance exit %d: %s", code, stderr.String())
	}
	if status, _, _ := get(t, front.Client(), front.URL, testHost, "/"); status != 503 {
		t.Fatalf("maintenance not applied: %d", status)
	}
	code, out, _ = run("status")
	if code != ExitOK || !strings.Contains(out, `"maintenance":true`) {
		t.Fatalf("status exit %d: %s", code, out)
	}
	if code, _, _ := run("bogus"); code != ExitUsage {
		t.Fatalf("unknown command exit %d", code)
	}
	var version, versionErr bytes.Buffer
	if code := Main(ctx, []string{"version"}, &version, &versionErr); code != ExitOK || !strings.HasPrefix(version.String(), "forge-proxy ") || !strings.Contains(version.String(), "go1.") {
		t.Fatalf("version exit %d: %q", code, version.String())
	}
	if code := Main(ctx, []string{"version", "extra"}, &version, &versionErr); code != ExitUsage || !strings.Contains(versionErr.String(), "accepts no arguments") {
		t.Fatalf("version with an argument exit %d: %q", code, versionErr.String())
	}
	if code := Main(ctx, []string{"healthcheck", "-control", filepath.Join(dir, "missing.sock")}, &stdout, &stderr); code != ExitFailed {
		t.Fatalf("healthcheck without proxy exit %d", code)
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	dir, err := os.MkdirTemp("", "fpr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx, cancel := context.WithCancel(t.Context())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"run", "-http-addr", "127.0.0.1:0", "-https-addr", "127.0.0.1:0",
			"-control", filepath.Join(dir, "c.sock"), "-state", filepath.Join(dir, "state.json"),
			"-acme-cache", filepath.Join(dir, "acme")}, &stdout, &stderr)
	}()
	if !waittest.Poll(5*time.Second, func() bool {
		response, err := Call(ctx, filepath.Join(dir, "c.sock"), ControlRequest{Op: "status"})
		return err == nil && response.OK
	}) {
		t.Fatalf("proxy did not start: %s", stderr.String())
	}
	cancel()
	if code := <-done; code != ExitOK {
		t.Fatalf("run exit %d: %s", code, stderr.String())
	}
}
