package sqldb_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestOperation(t *testing.T) {
	for query, want := range map[string]string{
		"SELECT 1":                          "SELECT",
		"  select * from users":             "SELECT",
		"\n\tInsert INTO t VALUES (1)":      "INSERT",
		"-- comment\nUPDATE t SET a = 1":    "UPDATE",
		"/* hint */ DELETE FROM t":          "DELETE",
		"(SELECT 1) UNION (SELECT 2)":       "SELECT",
		"WITH x AS (SELECT 1) SELECT 1":     "WITH",
		"secret_function('password')":       "OTHER",
		"selectx":                           "OTHER",
		"":                                  "OTHER",
		"-- ping":                           "OTHER",
		"/* unterminated":                   "OTHER",
		"PRAGMA foreign_keys":               "PRAGMA",
		"SELECTSELECTSELECTSELECT password": "OTHER",
	} {
		if got := sqldb.Operation(query); got != want {
			t.Errorf("Operation(%q) = %q, want %q", query, got, want)
		}
	}
	allocs := testing.AllocsPerRun(100, func() { _ = sqldb.Operation("/* c */ select 1") })
	if allocs != 0 {
		t.Fatalf("Operation allocated %v times", allocs)
	}
}

func TestKindNamesAreStable(t *testing.T) {
	t.Parallel()
	for kind, want := range map[sqldb.Kind]string{
		sqldb.KindUnknown:             "other",
		sqldb.KindNotFound:            "not_found",
		sqldb.KindUniqueViolation:     "unique_violation",
		sqldb.KindForeignKeyViolation: "foreign_key_violation",
		sqldb.KindExclusionViolation:  "exclusion_violation",
		sqldb.KindInvalidData:         "invalid_data",
		sqldb.KindConflict:            "conflict",
		sqldb.KindTimeout:             "timeout",
		sqldb.KindUnavailable:         "unavailable",
		sqldb.Kind(99):                "other",
	} {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d) = %q, want %q", kind, got, want)
		}
	}
}

func TestNewObserverRequiresASystemAndAClassifier(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	classify := func(error) sqldb.Classification { return sqldb.Classification{} }
	if _, err := sqldb.NewObserver(tel, "", classify); err == nil {
		t.Error("an empty database system was accepted")
	}
	if _, err := sqldb.NewObserver(tel, "mysql", nil); err == nil {
		t.Error("a nil classifier was accepted")
	}
}

func TestObservedErrorsAreLabelledByCauseAndClassification(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	observer, err := sqldb.NewObserver(tel, "mysql", func(err error) sqldb.Classification {
		if err.Error() == "duplicate" {
			return sqldb.Classification{Kind: sqldb.KindUniqueViolation}
		}
		return sqldb.Classification{}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, outcome := range []error{
		nil,
		context.Canceled,
		fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		errors.New("duplicate"),
		errors.New("anything else"),
		driver.ErrSkip, // only tells database/sql to take another path
	} {
		observer.Start(ctx, "SELECT 1").End(outcome)
	}
	series := telemetrytest.Series(tel, sqldb.MetricDBOperationDuration)
	for _, errorType := range []string{"", "canceled", "timeout", "unique_violation", "other"} {
		key := "db.system.name=mysql,db.operation.name=SELECT,error.type=" + errorType
		if series[key] != 1 {
			t.Errorf("%q measured %v times, want 1 (all: %v)", key, series[key], series)
		}
	}
	total := 0.0
	for _, count := range series {
		total += count
	}
	if total != 5 {
		t.Errorf("%v measurements, want 5: ErrSkip must not be recorded", total)
	}
}
