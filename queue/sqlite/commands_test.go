package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuesqlite "github.com/rocketinsidecorp/forge-go/queue/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
)

type commandRun struct {
	code           int
	stdout, stderr string
}

func runJobs(t *testing.T, url string, args ...string) commandRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	lookup := func(key string) (string, bool) {
		if key == "FORGE_DATABASE_URL" {
			return url, true
		}
		return "", false
	}
	code := forge.Execute(context.Background(), append([]string{"jobs"}, args...), lookup, &stdout, &stderr,
		func(context.Context, *forge.App) error { return nil }, queuesqlite.Commands()...)
	return commandRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestJobsCommand(t *testing.T) {
	db, cfg := sqlitetest.NewWithConfig(t)
	dbtest.Migrate(t, queuesqlite.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return sqlite.NewMigrator(cfg, loaded, logger)
	})
	store, err := queuesqlite.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	url := cfg.URL.Reveal()
	ctx := context.Background()

	registry := queue.NewRegistry()
	if err := queue.Register(registry, "report.build", func(context.Context, order) error {
		return queue.Permanent(errors.New("template missing"))
	}, queue.HandlerMaxAttempts(1)); err != nil {
		t.Fatal(err)
	}
	result, err := queue.Enqueue(ctx, store, registry, "report.build", order{ID: "r-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := runJobs(t, url, "status"); got.code != 0 || !strings.Contains(got.stdout, "default") || !strings.Contains(got.stdout, "READY") {
		t.Fatalf("status = %+v", got)
	}
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{PollInterval: 10 * time.Millisecond, MaxPollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waittest.Until(t, 15*time.Second, "the job to die", func() bool {
		info, err := store.Get(ctx, result.ID)
		return err == nil && info.State == queue.StateDead
	})
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := worker.Shutdown(stop); err != nil {
		t.Fatal(err)
	}

	id := result.ID.String()
	dead := runJobs(t, url, "dead")
	if dead.code != 0 || !strings.Contains(dead.stdout, id) || !strings.Contains(dead.stdout, "template missing") || strings.Contains(dead.stdout, "r-1") {
		t.Fatalf("dead = %+v", dead)
	}
	if got := runJobs(t, url, "dead", "-kind", "other"); !strings.Contains(got.stdout, "no dead jobs") {
		t.Fatalf("filtered dead = %+v", got)
	}
	show := runJobs(t, url, "show", id)
	if show.code != 0 || !strings.Contains(show.stdout, "state:        dead") || strings.Contains(show.stdout, "r-1") {
		t.Fatalf("show = %+v", show)
	}
	if got := runJobs(t, url, "retry", id); got.code != 0 || !strings.Contains(got.stdout, "retrying") {
		t.Fatalf("retry = %+v", got)
	}
	if info, _ := store.Get(ctx, result.ID); info.State != queue.StatePending {
		t.Fatalf("state after retry = %s", info.State)
	}
	if got := runJobs(t, url, "retry", id); got.code != 1 || !strings.Contains(got.stderr, "not dead") {
		t.Fatalf("second retry = %+v", got)
	}
	if got := runJobs(t, url, "prune", "-older", "1h"); got.code != 0 || !strings.Contains(got.stdout, "deleted 0") {
		t.Fatalf("prune = %+v", got)
	}

	// Usage errors exit 2 without touching the database.
	for _, args := range [][]string{{}, {"nope"}, {"retry"}, {"retry", "not-an-id"}, {"dead", "-limit", "0"}, {"status", "extra"}, {"prune", "-older", "0s"}} {
		if got := runJobs(t, url, args...); got.code != 2 {
			t.Errorf("jobs %v exit code = %d, want 2 (%s)", args, got.code, got.stderr)
		}
	}

	// Discard needs a dead job.
	if err := store.Fail(ctx, queue.Lease{}, queue.Failure{}); err == nil {
		t.Fatal("Fail on an unknown lease succeeded")
	}
	other, err := queue.Enqueue(ctx, store, registry, "report.build", order{ID: "r-2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := runJobs(t, url, "discard", other.ID.String()); got.code != 1 {
		t.Fatalf("discard of a pending job = %+v", got)
	}
}
