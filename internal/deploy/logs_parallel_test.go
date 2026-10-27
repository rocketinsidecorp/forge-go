package deploy

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// streamCounter is a Runner whose Stream holds until holdUntil streams have
// been open at once, or until hold has passed, and records the peak of open streams.
type streamCounter struct {
	fakeRunner
	smu       sync.Mutex
	cond      *sync.Cond
	active    int
	peak      int
	total     int
	released  bool
	holdUntil int
	hold      time.Duration
}

func newStreamCounter(holdUntil int, hold time.Duration) *streamCounter {
	c := &streamCounter{holdUntil: holdUntil, hold: hold}
	c.cond = sync.NewCond(&c.smu)
	return c
}

func (c *streamCounter) Stream(ctx context.Context, _ string, _ Command, _ io.Writer) error {
	c.smu.Lock()
	c.active++
	c.total++
	c.peak = max(c.peak, c.active)
	c.released = c.released || c.active >= c.holdUntil
	c.cond.Broadcast()
	deadline := time.AfterFunc(c.hold, func() { c.cond.Broadcast() })
	start := time.Now()
	for !c.released && time.Since(start) < c.hold && ctx.Err() == nil {
		c.cond.Wait()
	}
	deadline.Stop()
	c.active--
	c.cond.Broadcast()
	c.smu.Unlock()
	return nil
}

// `forge logs` reads at most Parallelism hosts at once, except with --follow:
// a followed stream never ends, so every host streams at once (forge-go#43).
func TestLogsRespectParallelismUnlessFollowing(t *testing.T) {
	hosts := "[192.0.2.10, 192.0.2.11, 192.0.2.12, 192.0.2.13, 192.0.2.14]"
	config := strings.Replace(validConfig, "hosts: [192.0.2.10, 192.0.2.11]", "hosts: "+hosts, 1) + "\nparallelism: 2\n"
	for _, tt := range []struct {
		name     string
		follow   bool
		holdFor  int
		hold     time.Duration
		wantPeak int
	}{
		{"bounded without follow", false, 3, 300 * time.Millisecond, 2},
		{"every host at once with follow", true, 5, 10 * time.Second, 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, _, _, _ := planFixtureFor(t, false, config, nil)
			runner := newStreamCounter(tt.holdFor, tt.hold)
			d.Runner = runner
			if err := d.Logs(t.Context(), &bytes.Buffer{}, LogsOptions{Follow: tt.follow}); err != nil {
				t.Fatalf("Logs: %v", err)
			}
			runner.smu.Lock()
			defer runner.smu.Unlock()
			if runner.active != 0 || runner.total != 5 || runner.peak != tt.wantPeak {
				t.Fatalf("active=%d total=%d peak=%d, want 0, 5 and %d", runner.active, runner.total, runner.peak, tt.wantPeak)
			}
		})
	}
}

// Without a record, a held lock whose ln failed says what the remote said; the
// stale-takeover hint would be false there. A record, or no stderr, leaves the
// message alone, and secrets in the stderr are redacted.
func TestHeldLockWithoutRecordCarriesTheRemoteDiagnostic(t *testing.T) {
	t.Parallel()
	interrupted := parseLockInfo("")
	if got := heldLockInfo("", ""); got != interrupted {
		t.Fatalf("no stderr: %+v, want the interrupted-acquire message %+v", got, interrupted)
	}
	if got := heldLockInfo("{}\n", "  \n"); got != interrupted {
		t.Fatalf("blank stderr: %+v", got)
	}
	record := `{"holder":"bob@ci","message":"window","acquired_at":"2026-10-20T10:00:00Z"}`
	if got, want := heldLockInfo(record, "ln: noise"), parseLockInfo(record); got != want {
		t.Fatalf("a record with stderr: %+v, want %+v", got, want)
	}
	got := heldLockInfo("", "ln: failed to create hard link 'info': Operation not permitted\nsecond line")
	want := interrupted.Message + "; the remote said: ln: failed to create hard link 'info': Operation not permitted second line"
	if got.Holder != "unknown" || got.Message != want {
		t.Fatalf("message = %q, want %q", got.Message, want)
	}
	// A cut inside a multi-byte character goes back to the character before.
	runes := heldLockInfo("", strings.Repeat("x", maxHeldDetail-1)+strings.Repeat("é", 50))
	if !utf8.ValidString(runes.Message) || !strings.HasSuffix(runes.Message, strings.Repeat("x", maxHeldDetail-1)+"...") {
		t.Fatalf("the diagnostic was cut inside a character: %q", runes.Message[len(runes.Message)-20:])
	}
	long := heldLockInfo("", strings.Repeat("x", 2000))
	if !strings.HasSuffix(long.Message, strings.Repeat("x", maxHeldDetail)+"...") || len(long.Message) > len(interrupted.Message)+len("; the remote said: ")+maxHeldDetail+3 {
		t.Fatalf("the diagnostic is not bounded: %d bytes", len(long.Message))
	}
}

func TestAcquireReportsLnFailureOnAHeldLockWithoutRecord(t *testing.T) {
	d, runner, _, _ := planFixture(t, false)
	runner.fail = func(host string, cmd Command) error {
		if host == "192.0.2.10" && strings.Contains(cmd.Script, "if mkdir .forge/shop/lock 2>/dev/null") {
			return &RemoteError{Host: host, ExitCode: lockHeldExitStatus, Stderr: "ln: failed to create hard link: Operation not supported (registry-secret-1)"}
		}
		return nil
	}
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitLockHeld {
		t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
	text := err.Error()
	if !strings.Contains(text, "the remote said: ln: failed to create hard link: Operation not supported") {
		t.Fatalf("the ln diagnostic is missing: %s", text)
	}
	if strings.Contains(text, "registry-secret-1") {
		t.Fatalf("a secret leaked into the held message: %s", text)
	}
}
