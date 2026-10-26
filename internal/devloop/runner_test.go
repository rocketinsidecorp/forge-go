package devloop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

type fakeInstance struct {
	name string
	done chan struct{}
	once sync.Once
	log  func(string)
}

func (f *fakeInstance) Done() <-chan struct{} { return f.done }

func (f *fakeInstance) exit() { f.once.Do(func() { close(f.done) }) }

func (f *fakeInstance) Stop(time.Duration) error {
	f.log("stop " + f.name)
	f.exit()
	return nil
}

// harness runs a Runner with a fake build and a fake process, and records
// everything that happens in order.
type harness struct {
	t         *testing.T
	events    chan string
	changes   chan Change
	cancel    context.CancelFunc
	finished  chan error
	dir       string
	mu        sync.Mutex
	buildErr  func(n int) error // by build number from 1
	buildHook func(n int)
	builds    int
	instances map[string]*fakeInstance
	readyErr  func(name string) error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t: t, events: make(chan string, 100), changes: make(chan Change),
		finished: make(chan error, 1), dir: t.TempDir(), instances: map[string]*fakeInstance{},
	}
	return h
}

// start runs the runner; configure the harness first.
func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.t.Cleanup(cancel)
	runner := &Runner{
		Dir: h.dir, Name: "app",
		Build: func(_ context.Context, output string) error {
			h.mu.Lock()
			h.builds++
			n := h.builds
			h.mu.Unlock()
			h.log("build " + itoa(n))
			if h.buildHook != nil {
				h.buildHook(n)
			}
			if h.buildErr != nil {
				if err := h.buildErr(n); err != nil {
					return err
				}
			}
			return os.WriteFile(output, []byte("binary "+itoa(n)), 0o700)
		},
		Start: func(_ context.Context, binary string) (Instance, error) {
			name := filepath.Base(binary)
			h.log("start " + name)
			instance := &fakeInstance{name: name, done: make(chan struct{}), log: h.log}
			h.mu.Lock()
			h.instances[name] = instance
			h.mu.Unlock()
			return instance, nil
		},
		Ready: func(_ context.Context, instance Instance) error {
			if h.readyErr != nil {
				return h.readyErr(instance.(*fakeInstance).name)
			}
			return nil
		},
		StopGrace: time.Second,
		Observer: Observer{
			BuildFailed: func(output string) { h.log("build-failed " + output) },
			Stopping:    func() { h.log("stopping") },
			Started:     func(i Instance) { h.log("started " + i.(*fakeInstance).name) },
			Crashed:     func() { h.log("crashed") },
			StartFailed: func(err error) { h.log("start-failed " + err.Error()) },
		},
	}
	go func() { h.finished <- runner.Run(ctx, h.changes) }()
}

func itoa(n int) string { return string(rune('0' + n)) }

func (h *harness) log(event string) { h.events <- event }

func (h *harness) expect(want ...string) {
	h.t.Helper()
	for _, event := range want {
		select {
		case got := <-h.events:
			if got != event {
				h.t.Fatalf("event = %q, want %q", got, event)
			}
		case <-time.After(testWait):
			h.t.Fatalf("timed out waiting for %q", event)
		}
	}
}

func (h *harness) send(change Change) {
	h.t.Helper()
	select {
	case h.changes <- change:
	case <-time.After(testWait):
		h.t.Fatal("runner did not accept a change")
	}
}

func (h *harness) instance(name string) *fakeInstance {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.instances[name]
}

func (h *harness) stop() {
	h.t.Helper()
	h.cancel()
	select {
	case err := <-h.finished:
		if err != nil {
			h.t.Fatalf("Run: %v", err)
		}
	case <-time.After(testWait):
		h.t.Fatal("Run did not return after cancel")
	}
}

func TestRunnerStopsOldBeforeStartingNewAfterASuccessfulBuild(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.expect("build 1", "start app-1", "started app-1")

	h.send(Change{Count: 1, Paths: []string{"a.go"}})
	h.expect("build 2", "stopping", "stop app-1", "start app-2", "started app-2")
	if _, err := os.Stat(filepath.Join(h.dir, "app-1")); !os.IsNotExist(err) {
		t.Fatalf("the superseded binary must be removed, stat err = %v", err)
	}
	h.stop()
	h.expect("stop app-2")
}

func TestRunnerKeepsTheOldProcessRunningWhenTheBuildFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.buildErr = func(n int) error {
		if n == 2 {
			return &BuildError{Output: "main.go:3: undefined: x"}
		}
		return nil
	}
	h.start()
	h.expect("build 1", "start app-1", "started app-1")

	h.send(Change{Count: 1})
	h.expect("build 2", "build-failed main.go:3: undefined: x")
	select {
	case <-h.instance("app-1").done:
		t.Fatal("a failed build must not stop the running application")
	case <-time.After(100 * time.Millisecond):
	}
	if len(h.events) != 0 {
		t.Fatalf("nothing else may happen after a failed build, got %d events", len(h.events))
	}

	h.send(Change{Count: 1}) // the fix
	h.expect("build 3", "stopping", "stop app-1", "start app-3", "started app-3")
	h.stop()
	h.expect("stop app-3")
}

func TestRunnerSurvivesAFailedInitialBuildAndStartsOnTheFix(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.buildErr = func(n int) error {
		if n == 1 {
			return errors.New("syntax error")
		}
		return nil
	}
	h.start()
	h.expect("build 1", "build-failed syntax error")

	h.send(Change{Count: 1})
	h.expect("build 2", "start app-2", "started app-2")
	h.stop()
	h.expect("stop app-2")
}

func TestRunnerRestartsWithoutRebuildingForEnvOnlyChanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.expect("build 1", "start app-1", "started app-1")

	h.send(Change{Count: 1, EnvOnly: true})
	h.expect("stopping", "stop app-1", "start app-1", "started app-1")
	h.stop()
	h.expect("stop app-1")
}

func TestRunnerDiscardsABuildThatIsStaleBecauseTheTreeChangedMeanwhile(t *testing.T) {
	t.Parallel()
	// The runner polls for a newer change once, without blocking, when the
	// build ends: the sender must already be parked on the channel. A bubble
	// lets the test know that, where a sleep only hoped for it.
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		released := make(chan struct{})
		h.buildHook = func(n int) {
			if n == 1 {
				<-released
			}
		}
		h.start()
		h.expect("build 1")
		// The runner is busy building; the next change waits for it.
		sent := make(chan struct{})
		go func() {
			h.changes <- Change{Count: 1}
			close(sent)
		}()
		synctest.Wait() // the build is held and the sender is parked
		close(released)
		<-sent

		// The first build is stale: no process starts from it.
		h.expect("build 2", "start app-2", "started app-2")
		if _, err := os.Stat(filepath.Join(h.dir, "app-1")); !os.IsNotExist(err) {
			t.Fatalf("the stale binary must be removed, stat err = %v", err)
		}
		h.stop()
		h.expect("stop app-2")
	})
}

func TestRunnerDoesNotRestartACrashedApplicationUntilTheNextChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.expect("build 1", "start app-1", "started app-1")

	h.instance("app-1").exit()
	h.expect("crashed")
	select {
	case event := <-h.events:
		t.Fatalf("a crashed application must wait for a change, got %q", event)
	case <-time.After(100 * time.Millisecond):
	}

	h.send(Change{Count: 1})
	h.expect("build 2", "start app-2", "started app-2") // nothing to stop or announce
	h.stop()
	h.expect("stop app-2")
}

func TestRunnerReportsAnApplicationThatNeverBecomesReady(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.readyErr = func(name string) error {
		if name == "app-1" {
			return errors.New("not ready")
		}
		return nil
	}
	h.start()
	h.expect("build 1", "start app-1", "start-failed not ready")
	h.send(Change{Count: 1})
	h.expect("build 2", "stopping", "stop app-1", "start app-2", "started app-2")
	h.stop()
	h.expect("stop app-2")
}

func TestRunnerEndsWhenTheChangeChannelCloses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	h.expect("build 1", "start app-1", "started app-1")
	close(h.changes)
	select {
	case err := <-h.finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWait):
		t.Fatal("Run did not return")
	}
	h.expect("stop app-1")
}

func TestRunnerRequiresItsFunctions(t *testing.T) {
	t.Parallel()
	if err := (&Runner{}).Run(context.Background(), nil); err == nil {
		t.Fatal("want an error")
	}
}

func TestProcessStopsGracefullyOnSIGTERM(t *testing.T) {
	t.Parallel()
	ready := filepath.Join(t.TempDir(), "ready")
	script := writeScript(t, "trap 'exit 0' TERM\n: > \"$1\"\nwhile :; do sleep 0.05; done\n")
	process, err := StartProcess("/bin/sh", []string{script, ready}, t.TempDir(), os.Environ(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	if err := process.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := process.Err(); err != nil {
		t.Fatalf("a graceful exit must succeed: %v", err)
	}
}

func TestProcessIsKilledWhenItIgnoresSIGTERM(t *testing.T) {
	t.Parallel()
	ready := filepath.Join(t.TempDir(), "ready")
	script := writeScript(t, "trap '' TERM\n: > \"$1\"\nwhile :; do sleep 0.05; done\n")
	process, err := StartProcess("/bin/sh", []string{script, ready}, t.TempDir(), os.Environ(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	if err := process.Stop(200 * time.Millisecond); err == nil {
		t.Fatal("want an error reporting the kill")
	}
	select {
	case <-process.Done():
	default:
		t.Fatal("the process must be gone after Stop")
	}
}

func TestStartProcessReportsAMissingBinary(t *testing.T) {
	t.Parallel()
	if _, err := StartProcess(filepath.Join(t.TempDir(), "missing"), nil, t.TempDir(), nil, nil, nil); err == nil {
		t.Fatal("want an error")
	}
}

// waitForFile waits until the script reports that its signal handling is set.
func waitForFile(t *testing.T, name string) {
	t.Helper()
	waittest.Until(t, testWait, "the script to become ready", func() bool {
		_, err := os.Stat(name)
		return err == nil
	})
}

// writeScript writes a shell script for the tests to run as `/bin/sh
// script`. Executing the file itself races with parallel tests: a fork in
// another test can still hold the write descriptor, and exec then fails with
// "text file busy" (ETXTBSY).
func writeScript(t *testing.T, body string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "app.sh")
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}
