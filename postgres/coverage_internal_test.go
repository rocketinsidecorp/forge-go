package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestClassifyNamesTheKindsTelemetryAndTranslationRelyOn(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		err        error
		kind       sqldb.Kind
		constraint string
	}{
		"no rows":              {pgx.ErrNoRows, sqldb.KindNotFound, ""},
		"wrapped no rows":      {errors.Join(errors.New("get"), pgx.ErrNoRows), sqldb.KindNotFound, ""},
		"exclusion constraint": {&pgconn.PgError{Code: "23P01", ConstraintName: "bookings_during_excl"}, sqldb.KindExclusionViolation, "bookings_during_excl"},
		"unique":               {&pgconn.PgError{Code: "23505", ConstraintName: "u"}, sqldb.KindUniqueViolation, "u"},
	} {
		if got := classify(tt.err); got.Kind != tt.kind || got.Constraint != tt.constraint {
			t.Errorf("%s: classify = %+v, want kind %v constraint %q", name, got, tt.kind, tt.constraint)
		}
	}
}

// matchesValidatedURL is defense in depth behind config and pgurl: each
// disagreement between the validated URL and what pgx parsed is refused, and
// none of the errors quotes the URL.
func TestMatchesValidatedURLRefusesEveryDisagreement(t *testing.T) {
	t.Parallel()
	same := pgconn.Config{Host: "db.internal", Port: 5432}
	for name, tt := range map[string]struct {
		raw    string
		parsed pgconn.Config
		want   string
	}{
		"an invalid URL":       {"postgres://u:hunter2@h/%zz", same, "not a valid PostgreSQL connection string"},
		"a port out of range":  {"postgres://u:hunter2@db.internal:99999/db", same, "invalid port"},
		"another host":         {"postgres://u:hunter2@db.internal/db", pgconn.Config{Host: "elsewhere", Port: 5432}, "ambiguous"},
		"another port":         {"postgres://u:hunter2@db.internal/db", pgconn.Config{Host: "db.internal", Port: 5433}, "ambiguous"},
		"a fallback elsewhere": {"postgres://u:hunter2@db.internal/db", pgconn.Config{Host: "db.internal", Port: 5432, Fallbacks: []*pgconn.FallbackConfig{{Host: "elsewhere", Port: 5432}}}, "ambiguous"},
		"TLS required, absent": {"postgres://u:hunter2@db.internal/db?sslmode=verify-full", same, "ambiguous"},
	} {
		err := matchesValidatedURL(tt.raw, tt.parsed)
		if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tt.want)
		}
	}
	if err := matchesValidatedURL("postgres://u:pw@db.internal/db", same); err != nil {
		t.Errorf("a URL that matches: %v", err)
	}
	if err := matchesValidatedURL("postgres://u:pw@db.internal:5433/db", pgconn.Config{Host: "db.internal", Port: 5433}); err != nil {
		t.Errorf("a URL with a port that matches: %v", err)
	}
}

type fakeStarter struct {
	tx  pgx.Tx
	err error
}

func (f fakeStarter) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return f.tx, f.err }

type fakeRowSet struct {
	pgx.Rows
	rows    int
	scanErr error
	err     error
}

func (f *fakeRowSet) Next() bool {
	if f.rows == 0 {
		return false
	}
	f.rows--
	return true
}
func (f *fakeRowSet) Scan(...any) error { return f.scanErr }
func (f *fakeRowSet) Err() error        { return f.err }
func (f *fakeRowSet) Close()            {}

// fakeTx is a transaction whose statements fail or return what the test says.
type fakeTx struct {
	pgx.Tx
	execErr    error
	queryErr   error
	rows       *fakeRowSet
	nested     *fakeTx
	rolledBack bool
	executed   []string
}

func (f *fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.executed = append(f.executed, sql)
	return pgconn.CommandTag{}, f.execErr
}

func (f *fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return f.rows, f.queryErr
}

func (f *fakeTx) QueryRow(context.Context, string, ...any) pgx.Row { return fakeRow{} }

func (f *fakeTx) Begin(context.Context) (pgx.Tx, error) { return f.nested, nil }

func (f *fakeTx) Rollback(context.Context) error { f.rolledBack = true; return nil }

func (f *fakeTx) Commit(context.Context) error { return nil }

type fakeRow struct{}

func (fakeRow) Scan(dest ...any) error {
	for _, d := range dest {
		if text, ok := d.(*string); ok {
			*text = ""
		}
	}
	return nil
}

// The role check fails closed: a failure at any step of it, or a catalog that
// does not list the login role, refuses to serve rather than assuming the
// role is harmless.
func TestRefuseRowSecurityBypassFailsClosedAtEveryStep(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, tt := range map[string]struct {
		starter fakeStarter
		cause   error
	}{
		"the transaction cannot start": {fakeStarter{err: boom}, boom},
		"search_path cannot be pinned": {fakeStarter{tx: &fakeTx{execErr: boom}}, boom},
		"the query fails":              {fakeStarter{tx: &fakeTx{queryErr: boom}}, boom},
		"a row cannot be read":         {fakeStarter{tx: &fakeTx{rows: &fakeRowSet{rows: 1, scanErr: boom}}}, boom},
		"the rows end in an error":     {fakeStarter{tx: &fakeTx{rows: &fakeRowSet{err: boom}}}, boom},
		"the login role is not listed": {fakeStarter{tx: &fakeTx{rows: &fakeRowSet{}}}, ErrRowSecurityBypass},
	} {
		err := refuseRowSecurityBypass(context.Background(), tt.starter)
		if !errors.Is(err, tt.cause) {
			t.Errorf("%s: err = %v, want one wrapping %v", name, err, tt.cause)
		}
		if err == nil {
			continue
		}
		if tt.cause == boom && !strings.Contains(err.Error(), "check the database role's row-level security") {
			t.Errorf("%s: the error does not say what was being checked: %v", name, err)
		}
		if tx, ok := tt.starter.tx.(*fakeTx); ok && !tx.rolledBack {
			t.Errorf("%s: the transaction was left open", name)
		}
	}
}

// A tenant-scoped step inside a caller's transaction runs in a savepoint: if
// installing the tenant fails the step fails, the savepoint is rolled back and
// the step's function never runs.
func TestTenantScopedStepsFailWhenTheTenantCannotBeInstalled(t *testing.T) {
	t.Parallel()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	savepoint := &fakeTx{execErr: boom}
	column := encryptedColumn{db: &fakeTx{nested: savepoint}, tenant: &tenant}
	ran := false
	err = column.inTenantTx(context.Background(), func(DBTX) error { ran = true; return nil })
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "set transaction tenant") || ran || !savepoint.rolledBack {
		t.Errorf("inTenantTx = %v (ran=%v, savepoint rolled back=%v)", err, ran, savepoint.rolledBack)
	}
	if err := setTransactionTenant(context.Background(), savepoint, tenant); !errors.Is(err, boom) {
		t.Errorf("setTransactionTenant = %v", err)
	}
}

// EncryptedColumn refuses a tenant scope over anything that cannot open
// transactions; the step runner refuses it too, should one be built by hand.
func TestTenantScopedStepsRefuseAHandleThatCannotOpenTransactions(t *testing.T) {
	t.Parallel()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	column := encryptedColumn{db: noTransactions{}, tenant: &tenant}
	err = column.inTenantTx(context.Background(), func(DBTX) error { t.Error("the step ran"); return nil })
	if err == nil || !strings.Contains(err.Error(), "needs a *postgres.DB or a pgx.Tx") {
		t.Errorf("inTenantTx = %v", err)
	}
}

type noTransactions struct{ DBTX }

// When the server version cannot be read, the migration's own error is the
// one reported, unchanged.
func TestExplainScriptErrorKeepsTheErrorWhenTheVersionCannotBeRead(t *testing.T) {
	t.Parallel()
	config, err := pgx.ParseConfig("postgres://u:pw@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*config)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	missing := &pgconn.PgError{Code: "42883", Message: "function uuidv7() does not exist"}
	if got := (dialect{}).ExplainScriptError(context.Background(), db, missing); got != error(missing) {
		t.Errorf("ExplainScriptError = %v, want the migration's error unchanged", got)
	}
}
