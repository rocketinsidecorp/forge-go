package dbtest_test

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
)

// recorder captures how ServerURL ends a test without ending this one.
type recorder struct {
	testing.TB
	outcome string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.outcome = "fatal: " + fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.outcome = "skip: " + fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func serverURLOutcome(t *testing.T) string {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		dbtest.ServerURL(r, config.AdapterPostgres)
	}()
	<-done
	return r.outcome
}

// TestRequireMustBeABoolean is a regression test: "yes" did not parse and
// silently turned required database tests back into skips.
func TestRequireMustBeABoolean(t *testing.T) {
	t.Setenv(dbtest.EnvAdapterURL(config.AdapterPostgres), "")
	t.Setenv(dbtest.EnvURL, "")
	for value, want := range map[string]string{
		"yes":   "fatal: FORGE_TEST_REQUIRE_DATABASE must be true or false",
		"true":  "fatal: FORGE_TEST_POSTGRES_URL or FORGE_TEST_DATABASE_URL is required when FORGE_TEST_REQUIRE_DATABASE is set",
		"false": "skip: set FORGE_TEST_POSTGRES_URL to run postgres tests",
		"":      "skip: set FORGE_TEST_POSTGRES_URL to run postgres tests",
	} {
		t.Setenv(dbtest.EnvRequire, value)
		if got := serverURLOutcome(t); got != want {
			t.Errorf("%s=%q: got %q, want %q", dbtest.EnvRequire, value, got, want)
		}
	}
}

func unsupportedOutcome(t *testing.T) string {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		dbtest.Unsupported(r, "requires PostgreSQL %d (uuidv7()): this server is %s", 18, "16.4")
	}()
	<-done
	return r.outcome
}

// A server that cannot run a test skips it with the reason, and fails it
// with the reason when the database is required, so a CI job cannot silently
// stop running a suite.
func TestUnsupportedSkipsOrFailsWhenTheDatabaseIsRequired(t *testing.T) {
	const reason = "requires PostgreSQL 18 (uuidv7()): this server is 16.4"
	for value, want := range map[string]string{
		"":      "skip: " + reason,
		"false": "skip: " + reason,
		"true":  "fatal: " + reason + " (FORGE_TEST_REQUIRE_DATABASE is set)",
		"yes":   "fatal: FORGE_TEST_REQUIRE_DATABASE must be true or false",
	} {
		t.Setenv(dbtest.EnvRequire, value)
		if got := unsupportedOutcome(t); got != want {
			t.Errorf("%s=%q: got %q, want %q", dbtest.EnvRequire, value, got, want)
		}
	}
}
