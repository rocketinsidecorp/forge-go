package devloop

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedInstance is an Instance that records how it was stopped.
type scriptedInstance struct {
	done    chan struct{}
	once    sync.Once
	stopErr error
	grace   chan time.Duration
}

func newScriptedInstance() *scriptedInstance {
	return &scriptedInstance{done: make(chan struct{}), grace: make(chan time.Duration, 4)}
}

func (s *scriptedInstance) Done() <-chan struct{} { return s.done }
func (s *scriptedInstance) exit()                 { s.once.Do(func() { close(s.done) }) }
func (s *scriptedInstance) Stop(grace time.Duration) error {
	s.grace <- grace
	s.exit()
	return s.stopErr
}

// runnerRun runs r until it returns, with changes fed from a buffered
// channel the test fills, and returns what Run returned.
func runnerRun(t *testing.T, r *Runner, ctx context.Context, changes <-chan Change) error {
	t.Helper()
	r.Dir, r.Name = t.TempDir(), "app"
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, changes) }()
	select {
	case err := <-done:
		return err
	case <-time.After(testWait):
		t.Fatal("Run did not return")
		return nil
	}
}

func okBuild(_ context.Context, output string) error {
	return os.WriteFile(output, []byte("bin"), 0o700)
}

func TestRunnerAnnouncesBuildsAndLogsProgress(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var logs []string
	building := 0
	instance := newScriptedInstance()
	changes := make(chan Change)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	r := &Runner{
		Build: okBuild,
		Start: func(context.Context, string) (Instance, error) { return instance, nil },
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, strings.TrimSpace(format))
		},
		Observer: Observer{
			Building: func() { building++ },
			Started:  func(Instance) { close(started) },
		},
	}
	go func() { <-started; cancel() }()
	if err := runnerRun(t, r, ctx, changes); err != nil {
		t.Fatal(err)
	}
	if building != 1 {
		t.Errorf("Building was called %d times", building)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(logs, "|"); got != "building|running" {
		t.Errorf("logs = %q", got)
	}
	// With no StopGrace set the default is used when the runner stops it.
	if grace := <-instance.grace; grace != DefaultStopGrace {
		t.Errorf("grace = %v, want %v", grace, DefaultStopGrace)
	}
}

func TestRunnerReportsAStopFailureAndKeepsGoing(t *testing.T) {
	t.Parallel()
	first, second := newScriptedInstance(), newScriptedInstance()
	first.stopErr = errors.New("application did not stop within 1s and was killed")
	instances := []*scriptedInstance{first, second}
	starts := 0
	var mu sync.Mutex
	var logs []string
	changes := make(chan Change, 1)
	ctx, cancel := context.WithCancel(context.Background())
	startedSecond := make(chan struct{})
	r := &Runner{
		Build: okBuild,
		Start: func(context.Context, string) (Instance, error) {
			starts++
			return instances[starts-1], nil
		},
		StopGrace: time.Second,
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, strings.TrimSpace(format))
		},
		Observer: Observer{Started: func(i Instance) {
			if i == Instance(first) {
				changes <- Change{Count: 1} // the next save, once the first is up
				return
			}
			close(startedSecond)
		}},
	}
	go func() { <-startedSecond; cancel() }()
	if err := runnerRun(t, r, ctx, changes); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(logs, "|"), "%v") {
		t.Errorf("the stop failure was not logged: %q", logs)
	}
	if grace := <-first.grace; grace != time.Second {
		t.Errorf("grace = %v", grace)
	}
}

func TestRunnerReportsAStartFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("exec format error")
	var failed error
	changes := make(chan Change)
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		Build: okBuild,
		Start: func(context.Context, string) (Instance, error) { return nil, boom },
		Observer: Observer{StartFailed: func(err error) {
			failed = err
			cancel()
		}},
	}
	if err := runnerRun(t, r, ctx, changes); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(failed, boom) || !strings.Contains(failed.Error(), "start application") {
		t.Errorf("StartFailed got %v", failed)
	}
}

func TestRunnerForgetsAnApplicationThatExitedWhileNotReady(t *testing.T) {
	t.Parallel()
	first, second := newScriptedInstance(), newScriptedInstance()
	instances := []*scriptedInstance{first, second}
	starts := 0
	var failures int
	changes := make(chan Change, 1)
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		Build: okBuild,
		Start: func(context.Context, string) (Instance, error) {
			starts++
			return instances[starts-1], nil
		},
		Ready: func(_ context.Context, instance Instance) error {
			if instance == Instance(first) {
				first.exit() // it died instead of serving
				return errors.New("exited before it was ready")
			}
			return nil
		},
		Observer: Observer{
			StartFailed: func(error) {
				failures++
				changes <- Change{Count: 1} // the fix
			},
			Stopping: func() { t.Error("an application that already exited is not stopped") },
			Started:  func(Instance) { cancel() },
		},
	}
	if err := runnerRun(t, r, ctx, changes); err != nil {
		t.Fatal(err)
	}
	if failures != 1 || starts != 2 {
		t.Errorf("failures %d, starts %d", failures, starts)
	}
	select {
	case <-first.grace:
		t.Error("the exited application was stopped")
	default:
	}
}

func TestRunnerStaysIdleAfterACrashUntilChangesCloseOrTheContextEnds(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(cancel context.CancelFunc, changes chan Change){
		"changes close": func(_ context.CancelFunc, changes chan Change) { close(changes) },
		"context ends":  func(cancel context.CancelFunc, _ chan Change) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			instance := newScriptedInstance()
			changes := make(chan Change)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			crashed := make(chan struct{})
			r := &Runner{
				Build:    okBuild,
				Start:    func(context.Context, string) (Instance, error) { return instance, nil },
				Observer: Observer{Started: func(Instance) { instance.exit() }, Crashed: func() { close(crashed) }},
			}
			go func() { <-crashed; end(cancel, changes) }()
			if err := runnerRun(t, r, ctx, changes); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerEndsQuietlyWhenTheContextEndsDuringABuild(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	failed := false
	r := &Runner{
		Build: func(context.Context, string) error {
			cancel() // the developer pressed Ctrl-C while the compiler ran
			return errors.New("signal: killed")
		},
		Start:    func(context.Context, string) (Instance, error) { t.Error("nothing starts"); return nil, nil },
		Observer: Observer{BuildFailed: func(string) { failed = true }},
	}
	if err := runnerRun(t, r, ctx, make(chan Change)); err != nil {
		t.Fatal(err)
	}
	if failed {
		t.Error("a build killed by Ctrl-C is not a build failure")
	}
}

func TestRunnerDropsAStaleBuildWhenTheContextEndedMeanwhile(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan Change, 1)
	changes <- Change{Count: 1} // already waiting when the first build finishes: it is stale
	builds := 0
	r := &Runner{
		Build: func(_ context.Context, output string) error {
			builds++
			cancel()
			return okBuild(context.Background(), output)
		},
		Start: func(context.Context, string) (Instance, error) { t.Error("nothing starts"); return nil, nil },
	}
	if err := runnerRun(t, r, ctx, changes); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Errorf("builds = %d: a cancelled run must not build again", builds)
	}
}

// --- hub ---

func TestHubUnsubscribeIsIdempotentAndBroadcastNeverBlocks(t *testing.T) {
	t.Parallel()
	h := newHub()
	events, unsubscribe, ok := h.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	// A client that never reads: the extra events are dropped, not queued.
	for i := range clientBuffer + 5 {
		h.broadcast(event{name: "reload", data: string(rune('a' + i))})
	}
	got := 0
	for range clientBuffer {
		select {
		case <-events:
			got++
		default:
		}
	}
	if got != clientBuffer {
		t.Errorf("received %d events, want %d", got, clientBuffer)
	}
	unsubscribe()
	if _, open := <-events; open {
		t.Error("unsubscribe must close the stream")
	}
	unsubscribe() // again: no panic from a second close
	h.close()
	if _, _, ok := h.subscribe(); ok {
		t.Error("a closed hub refuses subscribers")
	}
}

// --- proxy ---

// recordingWriter is a ResponseWriter whose Write fails after n successes
// and that announces every write.
type recordingWriter struct {
	header http.Header
	okLeft int
	wrote  chan struct{}
}

func newRecordingWriter(okWrites int) *recordingWriter {
	return &recordingWriter{header: http.Header{}, okLeft: okWrites, wrote: make(chan struct{}, 16)}
}

func (w *recordingWriter) Header() http.Header { return w.header }
func (w *recordingWriter) WriteHeader(int)     {}
func (w *recordingWriter) Flush()              {}
func (w *recordingWriter) Write(p []byte) (int, error) {
	defer func() { w.wrote <- struct{}{} }()
	if w.okLeft <= 0 {
		return 0, errors.New("broken pipe")
	}
	w.okLeft--
	return len(p), nil
}

func serveEventsTo(t *testing.T, p *Proxy, w http.ResponseWriter, ctx context.Context) <-chan struct{} {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, eventsPath, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { p.serveEvents(w, request); close(done) }()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatalf("%s did not end", what)
	}
}

func TestEventStreamEndsWhenABrowserStopsReading(t *testing.T) {
	t.Parallel()
	newProxy := func() *Proxy {
		p, err := NewProxy(ProxyOptions{Environment: "development", ShowBuildErrors: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	t.Run("the greeting cannot be written", func(t *testing.T) {
		t.Parallel()
		w := newRecordingWriter(0)
		waitDone(t, serveEventsTo(t, newProxy(), w, context.Background()), "the stream")
	})
	t.Run("the pending build error cannot be written", func(t *testing.T) {
		t.Parallel()
		p := newProxy()
		p.BuildFailed("main.go:1: boom")
		w := newRecordingWriter(1)
		waitDone(t, serveEventsTo(t, p, w, httptest.NewRequest(http.MethodGet, "/", nil).Context()), "the stream")
	})
	t.Run("a reload cannot be written", func(t *testing.T) {
		t.Parallel()
		p := newProxy()
		w := newRecordingWriter(1)
		done := serveEventsTo(t, p, w, context.Background())
		<-w.wrote // the greeting: the stream is subscribed
		p.Reload()
		waitDone(t, done, "the stream")
	})
	t.Run("the browser goes away", func(t *testing.T) {
		t.Parallel()
		p := newProxy()
		w := newRecordingWriter(10)
		ctx, cancel := context.WithCancel(context.Background())
		done := serveEventsTo(t, p, w, ctx)
		<-w.wrote
		cancel()
		waitDone(t, done, "the stream")
		// Its subscription was released: the hub has room again.
		for range maxClients {
			if _, _, ok := p.hub.subscribe(); !ok {
				t.Fatal("the stream did not unsubscribe")
			}
		}
	})
}

func TestServeReturnsAListenerFailure(t *testing.T) {
	t.Parallel()
	p, err := NewProxy(ProxyOptions{Environment: "development"})
	if err != nil {
		t.Fatal(err)
	}
	l := &failingListener{err: errors.New("accept tcp: too many open files")}
	if err := p.Serve(context.Background(), l); err == nil || !strings.Contains(err.Error(), "too many open files") {
		t.Fatalf("err = %v", err)
	}
}

type failingListener struct {
	err error
}

func (l *failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *failingListener) Close() error              { return nil }
func (l *failingListener) Addr() net.Addr            { return nil }

func TestProxyLogsUpstreamFailuresAndSkipsHEADInjection(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var logs []string
	p, err := NewProxy(ProxyOptions{Environment: "development", Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	p.SetUpstream("127.0.0.1:1") // nothing listens
	recorder := httptest.NewRecorder()
	p.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "The application is not answering") {
		t.Errorf("status %d body %q", recorder.Code, recorder.Body)
	}
	mu.Lock()
	if len(logs) != 1 || logs[0] != "proxy: %v" {
		t.Errorf("logs = %q", logs)
	}
	mu.Unlock()

	f := newFixture(t, servePage(page))
	request, _ := http.NewRequest(http.MethodHead, f.server.URL+"/", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Length") != "" && response.Header.Get("Content-Length") != strconv.Itoa(len(page)) {
		t.Errorf("HEAD: %d, Content-Length %q (the script must not change it)", response.StatusCode, response.Header.Get("Content-Length"))
	}
}

// --- process ---

func TestProcessStopOfAnExitedProcessIsANoOpAndIsolatedCommandsCancelBeforeStart(t *testing.T) {
	t.Parallel()
	process, err := StartProcess("/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), os.Environ(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-process.Done()
	if err := process.Stop(time.Second); err != nil {
		t.Errorf("Stop of an exited process: %v", err)
	}

	cmd := exec.Command("/bin/true")
	IsolateCommand(cmd)
	if cmd.Cancel == nil {
		t.Fatal("IsolateCommand must install Cancel")
	}
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Cancel before the command started: %v", err)
	}
}

// --- watch ---

func TestWatchUsesCustomRulesAndScanSkipsSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "main.go", "package main")
	write(t, root, "notes.txt", "x")
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.go")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	files, err := scan(root, DefaultRules(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["main.go"]; !ok {
		t.Errorf("main.go is not watched: %v", files)
	}
	if _, ok := files["link.go"]; ok {
		t.Error("a symlink must not be followed or watched")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	changes, err := Watch(ctx, Options{Root: root, PollInterval: testPoll, Debounce: testDebounce, Rules: &Rules{Extensions: []string{".txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "main.go", "package main // edited: not a .txt, ignored by these rules")
	expectQuiet(t, changes, 5*testDebounce)
	write(t, root, "notes.txt", "changed")
	if change := expectChange(t, changes); len(change.Paths) != 1 || change.Paths[0] != "notes.txt" {
		t.Errorf("change = %+v", change)
	}
}
