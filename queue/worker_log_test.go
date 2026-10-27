package queue

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"sync"
	"testing"

	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// lockedBuffer is a bytes.Buffer the worker's goroutines and the test share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runOneJob runs the single job oneJob gives to completion and returns what a
// pretty logger at the given level wrote about it.
func runOneJob(t *testing.T, level slog.Level, opts WorkerOptions) string {
	t.Helper()
	logs := &lockedBuffer{}
	opts.Logger = slog.New(telemetry.NewPrettyHandler(logs, telemetry.PrettyOptions{Level: level}))
	finished := make(chan Result, 1)
	opts.Hooks = Hooks{Finished: func(r Result) { finished <- r }}
	reg := registerK(t, func(context.Context) error { return nil })
	w := newTestWorker(t, &fakeStore{claim: oneJob()}, reg, opts)
	defer shutdownQuietly(w)
	if result := <-finished; result.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome %s", result.Outcome)
	}
	return logs.String()
}

var jobLine = regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d\.\d{3}  JOB   k succeeded  \S+  queue=default attempt=1\n`)

// A successful job is logged with its queue, attempt and duration at Info
// when the application asks (development) and at Debug otherwise, so a
// production worker's log is not a line per job.
func TestWorkerLogsSuccessesAtInfoOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	if got := runOneJob(t, slog.LevelInfo, WorkerOptions{LogSuccesses: true}); !jobLine.MatchString(got) {
		t.Fatalf("no JOB line in %q", got)
	}
	if got := runOneJob(t, slog.LevelInfo, WorkerOptions{}); jobLine.MatchString(got) {
		t.Fatalf("a success was logged at Info: %q", got)
	}
	if got := runOneJob(t, slog.LevelDebug, WorkerOptions{}); !jobLine.MatchString(got) {
		t.Fatalf("a success is logged at Debug: %q", got)
	}
}
