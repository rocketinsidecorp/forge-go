package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/postgres"
)

func TestTranslate(t *testing.T) {
	t.Parallel()
	pgError := func(code, constraint string) error {
		return fmt.Errorf("insert task: %w", &pgconn.PgError{Code: code, ConstraintName: constraint, Message: "secret detail"})
	}
	existing := fault.New("task_not_found", "task not found", fault.CategoryNotFound, 0)
	tests := []struct {
		name          string
		err           error
		wantCode      string
		wantStatus    int
		wantRetryable bool
	}{
		{"no rows", fmt.Errorf("get: %w", pgx.ErrNoRows), "not_found", http.StatusNotFound, false},
		{"unique", pgError("23505", "tasks_title_key"), "already_exists", http.StatusConflict, false},
		{"foreign key", pgError("23503", "tasks_project_fkey"), "reference_violation", http.StatusConflict, false},
		{"check", pgError("23514", "tasks_title_check"), "constraint_violation", http.StatusBadRequest, false},
		{"not null is a server bug", pgError("23502", ""), "internal_error", http.StatusInternalServerError, false},
		{"bad cast is a server bug", pgError("22P02", ""), "internal_error", http.StatusInternalServerError, false},
		{"value too long", pgError("22001", ""), "constraint_violation", http.StatusBadRequest, false},
		{"serialization", pgError("40001", ""), "transaction_conflict", http.StatusConflict, true},
		{"deadlock", pgError("40P01", ""), "transaction_conflict", http.StatusConflict, true},
		{"statement timeout", pgError("57014", ""), "timeout", http.StatusServiceUnavailable, true},
		{"too many connections", pgError("53300", ""), "database_unavailable", http.StatusServiceUnavailable, true},
		{"connection exception", pgError("08006", ""), "database_unavailable", http.StatusServiceUnavailable, true},
		{"deadline", context.DeadlineExceeded, "timeout", http.StatusServiceUnavailable, true},
		{"syntax error", pgError("42601", ""), "internal_error", http.StatusInternalServerError, false},
		{"unknown", errors.New("boom"), "internal_error", http.StatusInternalServerError, false},
		{"already a fault", existing, "task_not_found", http.StatusNotFound, false},
	}
	for _, test := range tests {
		translated := fault.From(postgres.Translate(test.err))
		if translated == nil {
			t.Fatalf("%s: not translated to a fault", test.name)
		}
		if translated.Code != test.wantCode || translated.Status() != test.wantStatus || translated.Retryable != test.wantRetryable {
			t.Errorf("%s: got code=%s status=%d retryable=%v", test.name, translated.Code, translated.Status(), translated.Retryable)
		}
		if strings.Contains(translated.Message, "secret") {
			t.Errorf("%s: database message leaked: %s", test.name, translated.Message)
		}
		if test.name != "already a fault" && !errors.Is(translated, test.err) && !errors.Is(translated.Unwrap(), test.err) {
			t.Errorf("%s: original error lost from the chain", test.name)
		}
	}
	if unique := fault.From(postgres.Translate(pgError("23505", "tasks_title_key"))); unique.Metadata["constraint"] != "tasks_title_key" {
		t.Errorf("constraint metadata missing: %+v", unique.Metadata)
	}
	if postgres.Translate(nil) != nil {
		t.Error("nil must stay nil")
	}
}

func TestLockKeyIsStable(t *testing.T) {
	t.Parallel()
	first, again, other := postgres.LockKey("a"), postgres.LockKey("a"), postgres.LockKey("b")
	if first != again || first == other {
		t.Fatal("LockKey must be deterministic and discriminating")
	}
}

func TestOpenRequiresConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := postgres.Open(context.Background(), config.Default().Database); err == nil {
		t.Fatal("expected error without FORGE_DATABASE_URL")
	}
	cfg := config.Default().Database
	cfg.URL = config.NewSecret("postgres://app:hunter2@127.0.0.1:1/app?sslmode=disable&connect_timeout=1")
	_, err := postgres.Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected connection failure")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("connection error leaked the password: %v", err)
	}
}

func TestLostConnectionsAreUnavailable(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"conn closed":    fmt.Errorf("query: %w", pgconn.ErrConnClosed),
		"network":        &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")},
		"unexpected EOF": fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF),
	} {
		translated := fault.From(postgres.Translate(err))
		if translated == nil || translated.Code != "database_unavailable" || translated.Status() != http.StatusServiceUnavailable || !translated.Retryable {
			t.Errorf("%s: got %+v", name, translated)
		}
	}
}

// TestOpenRefusesURLsPgxReadsDifferently is defense in depth behind config:
// even an unvalidated config.Database cannot make pgx connect elsewhere or
// without TLS than the URL appears to say.
func TestOpenRefusesURLsPgxReadsDifferently(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"postgres://db.internal?sslmode=verify-full&x=@10.0.0.9:5432/app",
		"postgres://app:pw@db:5432/app?sslmode=verify-full&sslmode=disable",
	} {
		cfg := config.Default().Database
		cfg.URL = config.NewSecret(raw)
		_, err := postgres.Open(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("%q: expected an ambiguity error before connecting, got %v", raw, err)
		}
	}
}
