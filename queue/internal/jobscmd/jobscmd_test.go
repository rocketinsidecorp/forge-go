package jobscmd_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/queue/internal/jobscmd"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// fakeAdmin is the queue's admin surface with scripted answers.
type fakeAdmin struct {
	stats    queue.Stats
	info     map[uuid.UUID]queue.Info
	dead     []queue.Info // listed in pages of the requested size
	statsErr error
	listErr  error
	retryErr error
	pruneErr error
	pruned   []int64
	retried  []uuid.UUID
	discards []uuid.UUID
	pageSize []int
}

func (a *fakeAdmin) Stats(context.Context) (queue.Stats, error) { return a.stats, a.statsErr }

func (a *fakeAdmin) Get(_ context.Context, id uuid.UUID) (queue.Info, error) {
	info, ok := a.info[id]
	if !ok {
		return queue.Info{}, queue.ErrNotFound
	}
	return info, nil
}

func (a *fakeAdmin) ListDead(_ context.Context, query queue.DeadQuery) (queue.DeadPage, error) {
	if a.listErr != nil {
		return queue.DeadPage{}, a.listErr
	}
	a.pageSize = append(a.pageSize, query.Limit)
	start := 0
	if query.After != nil {
		for i, item := range a.dead {
			if item.Job.ID == query.After.ID {
				start = i + 1
			}
		}
	}
	end := min(start+min(query.Limit, 2), len(a.dead)) // pages of at most two
	page := queue.DeadPage{Items: a.dead[start:end]}
	if end < len(a.dead) {
		last := a.dead[end-1]
		page.Next = &queue.DeadCursor{ID: last.Job.ID, FinishedAt: *last.FinishedAt}
	}
	return page, nil
}

func (a *fakeAdmin) Retry(_ context.Context, id uuid.UUID) error {
	a.retried = append(a.retried, id)
	return a.retryErr
}

func (a *fakeAdmin) Discard(_ context.Context, id uuid.UUID) error {
	a.discards = append(a.discards, id)
	return a.retryErr
}

func (a *fakeAdmin) Prune(context.Context, time.Duration, int) (int64, error) {
	if a.pruneErr != nil {
		return 0, a.pruneErr
	}
	n := a.pruned[0]
	a.pruned = a.pruned[1:]
	return n, nil
}

func run(t *testing.T, admin *fakeAdmin, closeErr error, args ...string) (stdout string, err error) {
	t.Helper()
	command := jobscmd.Commands(func(context.Context, config.Database) (queue.Admin, func() error, error) {
		return admin, func() error { return closeErr }, nil
	})[0]
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("postgres://u:p@127.0.0.1/db")
	var out, errOut bytes.Buffer
	err = command.Run(t.Context(), forge.CommandEnv{Config: cfg, Stdout: &out, Stderr: &errOut}, args)
	return out.String(), err
}

func deadJob(kind, lastError string, finished time.Time) queue.Info {
	return queue.Info{
		Job:        queue.Job{ID: uuid.MustNew(), Kind: kind, Queue: "default", Attempt: 3, MaxAttempts: 3},
		State:      queue.StateDead,
		LastError:  lastError,
		FinishedAt: &finished,
	}
}

func TestJobsCommandValidatesUsageBeforeConnecting(t *testing.T) {
	t.Parallel()
	connected := false
	command := jobscmd.Commands(func(context.Context, config.Database) (queue.Admin, func() error, error) {
		connected = true
		return &fakeAdmin{}, func() error { return nil }, nil
	})[0]
	if command.Name != "jobs" || !strings.Contains(command.Summary, "queue") {
		t.Fatalf("command = %+v", command)
	}
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("postgres://u:p@127.0.0.1/db")
	id := uuid.MustNew().String()
	for name, args := range map[string][]string{
		"no arguments":       nil,
		"unknown subcommand": {"explode"},
		"status with args":   {"status", "now"},
		"dead with args":     {"dead", "now"},
		"dead zero limit":    {"dead", "-limit", "0"},
		"dead huge limit":    {"dead", "-limit", "1001"},
		"prune with args":    {"prune", "now"},
		"prune negative age": {"prune", "-older", "-1h"},
		"unknown flag":       {"status", "-nope"},
		"show without id":    {"show"},
		"show two ids":       {"show", id, id},
		"show malformed id":  {"show", "not-a-uuid"},
		"retry malformed id": {"retry", "1234"},
		"discard without id": {"discard"},
	} {
		err := command.Run(t.Context(), forge.CommandEnv{Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, args)
		if !errors.Is(err, forge.ErrUsage) {
			t.Errorf("%s: err = %v, want a usage error", name, err)
		}
	}
	if connected {
		t.Error("a usage error opened a database connection")
	}
	// Without a database the command says what to set.
	err := command.Run(t.Context(), forge.CommandEnv{Config: config.Default(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, []string{"status"})
	if err == nil || !strings.Contains(err.Error(), "FORGE_DATABASE_URL") {
		t.Errorf("no database: %v", err)
	}
}

func TestJobsCommandOpenAndCloseFailuresAreReported(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("postgres://u:p@127.0.0.1/db")
	openErr := errors.New("cannot connect")
	command := jobscmd.Commands(func(context.Context, config.Database) (queue.Admin, func() error, error) {
		return nil, nil, openErr
	})[0]
	if err := command.Run(t.Context(), forge.CommandEnv{Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, []string{"status"}); !errors.Is(err, openErr) {
		t.Errorf("open failure: %v", err)
	}
	closeErr := errors.New("close failed")
	if _, err := run(t, &fakeAdmin{}, closeErr, "status"); !errors.Is(err, closeErr) {
		t.Errorf("close failure: %v", err)
	}
}

func TestJobsStatusPrintsATableOrSaysThereAreNoJobs(t *testing.T) {
	t.Parallel()
	out, err := run(t, &fakeAdmin{}, nil, "status")
	if err != nil || strings.TrimSpace(out) != "no jobs" {
		t.Fatalf("empty = %q, %v", out, err)
	}
	oldest := time.Now().Add(-90 * time.Second)
	admin := &fakeAdmin{stats: queue.Stats{Queues: []queue.QueueStats{
		{Queue: "default", Ready: 3, Scheduled: 1, Running: 2, Dead: 4, Succeeded: 5, OldestReadyAt: &oldest},
		{Queue: "mail\x1b[31m", Ready: 0},
	}}}
	out, err = run(t, admin, nil, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"QUEUE", "OLDEST READY", "default", "1m", `mail\u001b[31m`} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("a control character reached the terminal: %q", out)
	}
	if _, err := run(t, &fakeAdmin{statsErr: errors.New("db down")}, nil, "status"); err == nil {
		t.Error("a stats failure was swallowed")
	}
}

func TestJobsDeadPagesThroughTheListAndShortensErrors(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	long := strings.Repeat("word ", 40)
	admin := &fakeAdmin{dead: []queue.Info{
		deadJob("mail.send", long, finished),
		deadJob("mail.send", "short\nmultiline\terror", finished.Add(time.Minute)),
		deadJob("report.build", "x", finished.Add(2*time.Minute)),
	}}
	out, err := run(t, admin, nil, "dead", "-limit", "3")
	if err != nil {
		t.Fatal(err)
	}
	if len(admin.pageSize) != 2 || admin.pageSize[0] != 3 || admin.pageSize[1] != 1 {
		t.Errorf("page sizes = %v, want the remaining limit asked for each page", admin.pageSize)
	}
	for _, item := range admin.dead {
		if !strings.Contains(out, item.Job.ID.String()) {
			t.Errorf("job %s is missing:\n%s", item.Job.ID, out)
		}
	}
	if !strings.Contains(out, "...") || strings.Contains(out, long) {
		t.Errorf("a long error was not shortened:\n%s", out)
	}
	if !strings.Contains(out, `short\u000amultiline\u0009error`) || !strings.Contains(out, "2026-10-20T09:00:00Z") {
		t.Errorf("output = %s", out)
	}
	// No dead jobs, a job without a finish time, and a listing failure.
	if out, err := run(t, &fakeAdmin{}, nil, "dead"); err != nil || strings.TrimSpace(out) != "no dead jobs" {
		t.Errorf("empty = %q, %v", out, err)
	}
	unfinished := deadJob("k", "e", finished)
	unfinished.FinishedAt = nil
	admin = &fakeAdmin{dead: []queue.Info{unfinished}}
	if out, err := run(t, admin, nil, "dead", "-queue", "default", "-kind", "k"); err != nil || !strings.Contains(out, "-") {
		t.Errorf("unfinished = %q, %v", out, err)
	}
	if _, err := run(t, &fakeAdmin{listErr: errors.New("db down")}, nil, "dead"); err == nil {
		t.Error("a listing failure was swallowed")
	}
}

func TestJobsShowPrintsEveryKnownFieldWithoutThePayload(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	lease := finished.Add(time.Minute)
	job := queue.Job{ID: uuid.MustNew(), Kind: "mail.send", Queue: "default", Attempt: 2, MaxAttempts: 5, Priority: 7, RunAt: finished, EnqueuedAt: finished, Key: "once"}
	admin := &fakeAdmin{info: map[uuid.UUID]queue.Info{job.ID: {
		Job: job, State: queue.StateRunning, WorkerID: "worker-1", LeaseUntil: &lease, FinishedAt: &finished, LastError: "boom\x1b[0m",
	}}}
	out, err := run(t, admin, nil, "show", job.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id:", "mail.send", "attempts:     2 of 5", "priority:     7", "key:          once", "worker:       worker-1", "lease until:  2026-10-20T09:01:00Z", "finished at:", `boom\u001b[0m`} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if _, err := run(t, admin, nil, "show", uuid.MustNew().String()); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unknown job: %v", err)
	}
	// A bare job prints no optional lines.
	bare := queue.Job{ID: uuid.MustNew(), Kind: "k", Queue: "q"}
	out, err = run(t, &fakeAdmin{info: map[uuid.UUID]queue.Info{bare.ID: {Job: bare, State: queue.StatePending}}}, nil, "show", bare.ID.String())
	if err != nil || strings.Contains(out, "key:") || strings.Contains(out, "worker:") || strings.Contains(out, "last error:") {
		t.Errorf("bare = %q, %v", out, err)
	}
}

func TestJobsRetryDiscardAndPruneExplainTheirFailures(t *testing.T) {
	t.Parallel()
	id := uuid.MustNew()
	admin := &fakeAdmin{}
	if out, err := run(t, admin, nil, "retry", id.String()); err != nil || !strings.Contains(out, "retrying "+id.String()) || len(admin.retried) != 1 {
		t.Errorf("retry = %q, %v", out, err)
	}
	if out, err := run(t, admin, nil, "discard", id.String()); err != nil || !strings.Contains(out, "discarded "+id.String()) || len(admin.discards) != 1 {
		t.Errorf("discard = %q, %v", out, err)
	}
	for sub := range map[string]bool{"retry": true, "discard": true} {
		if _, err := run(t, &fakeAdmin{retryErr: queue.ErrNotFound}, nil, sub, id.String()); err == nil || !strings.Contains(err.Error(), "does not exist or is not dead") {
			t.Errorf("%s of a missing job: %v", sub, err)
		}
		if _, err := run(t, &fakeAdmin{retryErr: errors.New("db down")}, nil, sub, id.String()); err == nil || strings.Contains(err.Error(), "does not exist") {
			t.Errorf("%s with a database failure: %v", sub, err)
		}
	}
	if _, err := run(t, &fakeAdmin{retryErr: queue.ErrKeyInUse}, nil, "retry", id.String()); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Errorf("retry with the key in use: %v", err)
	}
	// Prune loops until a batch is not full.
	out, err := run(t, &fakeAdmin{pruned: []int64{1000, 1000, 7}}, nil, "prune", "-older", "1h")
	if err != nil || !strings.Contains(out, "deleted 2007 finished job(s)") {
		t.Errorf("prune = %q, %v", out, err)
	}
	if _, err := run(t, &fakeAdmin{pruneErr: errors.New("db down")}, nil, "prune"); err == nil {
		t.Error("a prune failure was swallowed")
	}
}
