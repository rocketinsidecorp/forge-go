package sqldb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// kindError carries the Classification a fake driver would report.
type kindError struct{ classification sqldb.Classification }

func (e kindError) Error() string { return "driver detail: password=hunter2" }

func classifyFake(err error) sqldb.Classification {
	var fake kindError
	if errors.As(err, &fake) {
		return fake.classification
	}
	return sqldb.Classification{}
}

func TestTranslateMapsEveryKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind       sqldb.Kind
		wantCode   string
		wantStatus int
		retryable  bool
	}{
		{sqldb.KindNotFound, "not_found", http.StatusNotFound, false},
		{sqldb.KindUniqueViolation, "already_exists", http.StatusConflict, false},
		{sqldb.KindForeignKeyViolation, "reference_violation", http.StatusConflict, false},
		{sqldb.KindExclusionViolation, "conflict", http.StatusConflict, false},
		{sqldb.KindInvalidData, "constraint_violation", http.StatusBadRequest, false},
		{sqldb.KindConflict, "transaction_conflict", http.StatusConflict, true},
		{sqldb.KindTimeout, "timeout", http.StatusServiceUnavailable, true},
		{sqldb.KindUnavailable, "database_unavailable", http.StatusServiceUnavailable, true},
		{sqldb.KindUnknown, "internal_error", http.StatusInternalServerError, false},
	}
	for _, test := range tests {
		cause := fmt.Errorf("query: %w", kindError{sqldb.Classification{Kind: test.kind, Constraint: "c"}})
		translated := fault.From(sqldb.Translate(cause, classifyFake))
		if translated == nil || translated.Code != test.wantCode || translated.Status() != test.wantStatus || translated.Retryable != test.retryable {
			t.Errorf("kind %d: got %+v", test.kind, translated)
			continue
		}
		if !errors.Is(translated, cause) && !errors.Is(translated.Unwrap(), cause) {
			t.Errorf("kind %d: cause lost", test.kind)
		}
		if translated.Message == cause.Error() {
			t.Errorf("kind %d: driver message exposed", test.kind)
		}
	}
	if got := fault.From(sqldb.Translate(sql.ErrNoRows, classifyFake)); got == nil || got.Code != "not_found" {
		t.Errorf("sql.ErrNoRows: %+v", got)
	}
	if sqldb.Translate(nil, classifyFake) != nil {
		t.Error("nil must stay nil")
	}
	existing := fault.New("task_not_found", "task not found", fault.CategoryNotFound, 0)
	if !errors.Is(sqldb.Translate(existing, classifyFake), existing) {
		t.Error("faults must pass through")
	}
}

func TestRetry(t *testing.T) {
	t.Parallel()
	conflict := errors.New("deadlock")
	retryable := func(err error) bool { return errors.Is(err, conflict) }

	calls := 0
	err := sqldb.Retry(context.Background(), 3, retryable, func() error {
		calls++
		if calls < 3 {
			return conflict
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}

	calls = 0
	permanent := errors.New("syntax error")
	if err := sqldb.Retry(context.Background(), 5, retryable, func() error { calls++; return permanent }); !errors.Is(err, permanent) || calls != 1 {
		t.Fatalf("non-retryable errors must not retry: err=%v calls=%d", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sqldb.Retry(ctx, 5, retryable, func() error { return conflict }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must stop retries: %v", err)
	}
}

func TestCloseWithin(t *testing.T) {
	t.Parallel()
	if err := sqldb.CloseWithin(context.Background(), func() error { return nil }, func() int { return 0 }); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := sqldb.CloseWithin(ctx, func() error { <-release; return nil }, func() int { return 2 })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}
