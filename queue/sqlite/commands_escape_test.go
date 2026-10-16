package sqlite_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	queuesqlite "github.com/rocketinsidecorp/forge-go/queue/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/sqlite/sqlitetest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// Stored text can come from a writer other than the worker (another tool, an
// older version); the jobs command must never pass terminal control sequences
// through to the operator's terminal.
func TestJobsCommandEscapesTerminalControlSequences(t *testing.T) {
	db, cfg := sqlitetest.NewWithConfig(t)
	dbtest.Migrate(t, queuesqlite.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return sqlite.NewMigrator(cfg, loaded, logger)
	})
	id := uuid.MustNew()
	const stamp = "2026-10-14 12:00:00.000000"
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO forge_jobs (id, queue, kind, payload, state, max_attempts, attempts, run_at, enqueued_at, finished_at, idempotency_key, locked_by, last_error)
		VALUES (?, 'default', 'evil', '{}', 'dead', 3, 3, ?, ?, ?, ?, ?, ?)`,
		id, stamp, stamp, stamp, "key\u009b2J", "worker\u202e", "boom \u009b31m red \u202e gnirts \x1b[2J"); err != nil {
		t.Fatal(err)
	}
	url := cfg.URL.Reveal()
	for _, args := range [][]string{{"dead"}, {"show", id.String()}} {
		got := runJobs(t, url, args...)
		if got.code != 0 {
			t.Fatalf("jobs %v = %+v", args, got)
		}
		for _, raw := range []string{"\u009b", "\u202e", "\x1b"} {
			if strings.Contains(got.stdout, raw) {
				t.Errorf("jobs %v printed the raw control character %q", args, raw)
			}
		}
		if !strings.Contains(got.stdout, `\u009b`) {
			t.Errorf("jobs %v did not show the escaped control character:\n%s", args, got.stdout)
		}
	}
}
