package mysql_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	queuemysql "github.com/rocketinsidecorp/forge-go/queue/mysql"
)

func runJobs(t *testing.T, url string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	lookup := func(key string) (string, bool) {
		if key == "FORGE_DATABASE_URL" {
			return url, true
		}
		return "", false
	}
	code = forge.Execute(context.Background(), append([]string{"jobs"}, args...), lookup, &out, &errOut,
		func(context.Context, *forge.App) error { return nil }, queuemysql.Commands()...)
	return code, out.String(), errOut.String()
}

// The jobs operator command opens its own pool from FORGE_DATABASE_URL, reads
// the queue and releases the pool.
func TestJobsCommandReadsTheQueueOfTheConfiguredDatabase(t *testing.T) {
	_, cfg := mysqltest.NewWithConfig(t)
	dbtest.Migrate(t, queuemysql.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return mysql.NewMigrator(cfg, loaded, logger)
	})
	code, stdout, stderr := runJobs(t, cfg.URL.Reveal(), "status")
	if code != 0 {
		t.Fatalf("jobs status = %d, stderr %s", code, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("jobs status printed nothing")
	}
	if code, stdout, _ := runJobs(t, cfg.URL.Reveal(), "dead"); code != 0 || !strings.Contains(stdout, "no dead jobs") {
		t.Errorf("jobs dead = %d %q", code, stdout)
	}
}

func TestJobsCommandReportsADatabaseItCannotReach(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runJobs(t, "mysql://nobody:nothing@127.0.0.1:1/none", "status")
	if code != 1 || stdout != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "jobs:") || strings.Contains(stderr, "nothing") {
		t.Errorf("stderr = %q: want the failure named and the password absent", stderr)
	}
}
