package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jobspostgres "github.com/rocketinsidecorp/forge-go/jobs/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// TestRoutesStartsNoBackgroundWorkAndNeedsNoDatabase: forge routes composes
// the generated application without serving it (app.Serving() is false), so
// the scheduler of a generated PostgreSQL application does not start and no
// tick reaches forge_job_schedules (audit N-C2), and its pool is lazy, so an
// unreachable database does not stop the command (N-D1).
//
// The application registers a job every second and keeps Configure busy for
// 2.5 s after JobsConfigure, so a scheduler started by routes would record at
// least one tick.
//
// It is not parallel: it sets FORGE_DATABASE_URL, and the build resolves
// dependencies offline from the module cache.
func TestRoutesStartsNoBackgroundWorkAndNeedsNoDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on a generated project")
	}
	if !databaseAvailable("postgresql") && os.Getenv("FORGE_TEST_REQUIRE_DATABASE") == "" {
		t.Skip("no PostgreSQL server for the generated application (FORGE_TEST_POSTGRES_URL)")
	}
	db := postgrestest.NewRLS(t, jobspostgres.Migrations())
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")

	dir := matrixApplicationFor(t, "postgresql", "job", [][]string{{"cleanup"}})
	path := filepath.Join(dir, "app", "bootstrap", "bootstrap.go")
	source := uncommentJobExample(t, readFile(t, path))
	for _, edit := range []struct{ old, new string }{
		{"\t\"net/http\"\n", "\t\"net/http\"\n\t\"time\"\n"},
		{"jobs.ScheduleSpec{Every: 24 * time.Hour}", "jobs.ScheduleSpec{Every: time.Second}"},
		{
			"\treturn JobsConfigure(app, jobspostgres.NewTickClaimer(database))\n",
			"\tif err := JobsConfigure(app, jobspostgres.NewTickClaimer(database)); err != nil {\n\t\treturn err\n\t}\n" +
				"\ttime.Sleep(2500 * time.Millisecond)\n\treturn nil\n",
		},
	} {
		if !strings.Contains(source, edit.old) {
			t.Fatalf("bootstrap.go has no %q:\n%s", edit.old, source)
		}
		source = strings.Replace(source, edit.old, edit.new, 1)
	}
	writeFile(t, path, source)

	routes := func(databaseURL string) string {
		t.Helper()
		t.Setenv("FORGE_DATABASE_URL", databaseURL)
		var output, stderr bytes.Buffer
		if err := runRoutesAt(nil, &output, &stderr, dir); err != nil {
			t.Fatalf("forge routes: %v\n%s", err, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("forge routes fell back to the source scan:\n%s", stderr.String())
		}
		return output.String()
	}
	if table := routes(db.RuntimeConfig.URL.Reveal()); !strings.Contains(table, "/v1/status") {
		t.Fatalf("forge routes did not list the application's routes:\n%s", table)
	}
	var ticks int
	if err := db.Admin.QueryRow(context.Background(), "SELECT count(*) FROM forge_job_schedules").Scan(&ticks); err != nil {
		t.Fatal(err)
	}
	if ticks != 0 {
		t.Fatalf("forge routes recorded %d scheduler ticks in forge_job_schedules; describing the application must start no job", ticks)
	}

	// Nothing listens on port 1: only a lazy pool lets Configure finish.
	if table := routes("postgres://app_runtime:app_runtime@127.0.0.1:1/unreachable?sslmode=disable"); !strings.Contains(table, "/v1/status") {
		t.Fatalf("forge routes without a reachable database:\n%s", table)
	}
}
