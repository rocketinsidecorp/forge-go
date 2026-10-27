package sqlidentity_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	modernc "modernc.org/sqlite"

	"github.com/rocketinsidecorp/forge-go/auth"
	authsqlite "github.com/rocketinsidecorp/forge-go/auth/sqlite"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// errInjected is the failure the fault driver returns.
var errInjected = errors.New("injected database failure")

// faults fails the failAt-th driver call (a statement, a BEGIN or a COMMIT)
// after it is armed, once.
type faults struct {
	calls  atomic.Int64
	failAt atomic.Int64
	fired  atomic.Bool
}

func (f *faults) arm(n int64) {
	f.calls.Store(0)
	f.fired.Store(false)
	f.failAt.Store(n)
}

func (f *faults) next() error {
	if f.failAt.Load() > 0 && f.calls.Add(1) == f.failAt.Load() {
		f.fired.Store(true)
		return errInjected
	}
	return nil
}

type faultConnector struct {
	base driver.Connector
	f    *faults
}

func (c faultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: conn, f: c.f}, nil
}

func (c faultConnector) Driver() driver.Driver { return c.base.Driver() }

type faultConn struct {
	driver.Conn
	f *faults
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.f.next(); err != nil {
		return nil, err
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.f.next(); err != nil {
		return nil, err
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.f.next(); err != nil {
		return nil, err
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return faultTx{Tx: tx, f: c.f}, nil
}

type faultTx struct {
	driver.Tx
	f *faults
}

func (t faultTx) Commit() error {
	if err := t.f.next(); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Tx.Commit()
}

// faultyStore is an identity store on a migrated SQLite database whose
// driver fails the call faults names.
func faultyStore(t *testing.T) (*authsqlite.Repository, *authsqlite.LoginThrottler, *faults, *sqldb.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "faults.db")
	cfg := config.Default().Database
	cfg.URL = config.NewSecret("sqlite:" + path)
	dbtest.Migrate(t, authsqlite.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return sqlite.NewMigrator(cfg, loaded, logger)
	})
	params := url.Values{"_foreign_keys": {"1"}, "_txlock": {"immediate"}, "_busy_timeout": {"5000"}, "_time_format": {"sqlite"}, "_timezone": {"UTC"}}
	base, err := modernc.NewConnector((&url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	f := &faults{}
	raw := sql.OpenDB(faultConnector{base: base, f: f})
	raw.SetMaxOpenConns(1)
	db, err := sqldb.New(raw, cfg, func(error) sqldb.Classification { return sqldb.Classification{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository, err := authsqlite.New(db)
	if err != nil {
		t.Fatal(err)
	}
	throttler, err := authsqlite.NewLoginThrottler(db, auth.DefaultLoginThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	return repository, throttler, f, db
}

// everyFailure runs op with each of its database calls failing in turn,
// requiring an error each time, until op runs without a failure: every
// error path after a database call returns the failure instead of carrying
// on.
func everyFailure(t *testing.T, f *faults, name string, op func() error) {
	t.Helper()
	for n := int64(1); n < 100; n++ {
		f.arm(n)
		err := op()
		if !f.fired.Load() {
			f.arm(0)
			if err != nil {
				t.Fatalf("%s without a failure: %v", name, err)
			}
			return
		}
		if err == nil {
			t.Fatalf("%s succeeded although database call %d failed", name, n)
		}
	}
	t.Fatalf("%s makes more than 100 database calls", name)
}

func TestEveryDatabaseFailureIsReturned(t *testing.T) {
	t.Parallel()
	repository, throttler, f, db := faultyStore(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	org, tenant, user, membership, role := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	exec(`INSERT INTO forge_organizations (id, name) VALUES (?, 'Acme')`, org)
	exec(`INSERT INTO forge_tenants (id, organization_id, slug) VALUES (?, ?, 'acme')`, tenant, org)
	exec(`INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES (?, 'a@example.com', 'a@example.com', 'h1')`, user)
	exec(`INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES (?, ?, ?)`, membership, user, tenant)
	exec(`INSERT INTO forge_roles (id, tenant_id, name) VALUES (?, ?, 'member')`, role, tenant)
	exec(`INSERT INTO forge_permissions (name) VALUES ('tasks:read')`)
	exec(`INSERT INTO forge_role_permissions (role_id, permission) VALUES (?, 'tasks:read')`, role)
	exec(`INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES (?, ?, ?)`, membership, role, tenant)
	future := time.Now().Add(time.Hour)
	digest := func(s string) auth.Digest {
		var d auth.Digest
		copy(d[:], s+"................................")
		return d
	}
	codes := make([]auth.Digest, auth.DefaultMFABackupCodeCount)
	for i := range codes {
		codes[i] = digest(string(rune('a' + i)))
	}
	var token auth.Token
	steps := []struct {
		name string
		op   func() error
	}{
		{"ReplacePasswordHash", func() error { _, err := repository.ReplacePasswordHash(ctx, user, "h1", "h2"); return err }},
		{"CreateSession", func() error {
			var err error
			token, err = repository.CreateSession(ctx, membership, future)
			return err
		}},
		{"ResolveSession (stale)", func() error {
			if _, err := db.ExecContext(ctx, `UPDATE forge_sessions SET last_seen_at = '2000-01-01 00:00:00.000000', expires_at = ?`, time.Now().Add(time.Hour).UTC().Format("2006-01-02 15:04:05.000000")); err != nil {
				return err
			}
			_, found, err := repository.ResolveSession(ctx, token.Digest())
			if err != nil {
				return err
			}
			if found {
				return errors.New("an idle session resolved")
			}
			if _, err := db.ExecContext(ctx, `UPDATE forge_sessions SET last_seen_at = ?`, time.Now().Add(-5*time.Minute).UTC().Format("2006-01-02 15:04:05.000000")); err != nil {
				return err
			}
			_, _, err = repository.ResolveSession(ctx, token.Digest())
			return err
		}},
		{"RevokeUserSessions", func() error { return repository.RevokeUserSessions(ctx, user) }},
		{"RecordSecurityEvent", func() error {
			return repository.RecordSecurityEvent(ctx, auth.SecurityEvent{Kind: auth.SecurityLoginSucceeded, Outcome: auth.SecurityOutcomeSucceeded})
		}},
		{"CreateRecovery", func() error { return repository.CreateRecovery(ctx, user, digest("r1"), future) }},
		{"InvalidateRecovery", func() error { return repository.InvalidateRecovery(ctx, digest("r1")) }},
		{"CreateRecovery again", func() error { return repository.CreateRecovery(ctx, user, digest("r2"), future) }},
		{"ConsumeRecovery", func() error {
			consumed, err := repository.ConsumeRecovery(ctx, digest("r2"), "h3")
			if err == nil && !consumed {
				return errors.New("not consumed")
			}
			return err
		}},
		{"PruneRecovery", func() error { _, err := repository.PruneRecovery(ctx, 10); return err }},
		{"BeginTOTPEnrollment", func() error { return repository.BeginTOTPEnrollment(ctx, user, digest("s1"), []byte("c1"), future) }},
		{"ConfirmTOTPEnrollment", func() error {
			confirmed, err := repository.ConfirmTOTPEnrollment(ctx, user, digest("s1"), 10, codes)
			if err == nil && !confirmed {
				return errors.New("not confirmed")
			}
			return err
		}},
		{"CreateMFAChallenge", func() error { return repository.CreateMFAChallenge(ctx, digest("c1"), membership, 4, future, future) }},
		{"ConsumeMFAChallenge", func() error {
			_, consumed, err := repository.ConsumeMFAChallenge(ctx, digest("c1"), digest("s1"), 11)
			if err == nil && !consumed {
				return errors.New("not consumed")
			}
			return err
		}},
		{"CreateMFAChallenge again", func() error { return repository.CreateMFAChallenge(ctx, digest("c2"), membership, 4, future, future) }},
		{"ConsumeMFABackupChallenge", func() error {
			_, consumed, err := repository.ConsumeMFABackupChallenge(ctx, digest("c2"), codes[0])
			if err == nil && !consumed {
				return errors.New("not consumed")
			}
			return err
		}},
		{"AdvanceTOTPCounter", func() error { _, err := repository.AdvanceTOTPCounter(ctx, user, digest("s1"), 20); return err }},
		{"ReplaceMFABackupCodes", func() error {
			replaced, err := repository.ReplaceMFABackupCodes(ctx, user, codes, &codes[1])
			if err == nil && !replaced {
				return errors.New("not replaced")
			}
			return err
		}},
		{"BeginTOTPRotation", func() error {
			return repository.BeginTOTPRotation(ctx, user, digest("s2"), []byte("c2"), future, &codes[2])
		}},
		{"ConfirmTOTPRotation", func() error {
			confirmed, err := repository.ConfirmTOTPRotation(ctx, user, digest("s2"), 30, codes, &codes[3])
			if err == nil && !confirmed {
				return errors.New("not confirmed")
			}
			return err
		}},
		{"DisableMFA", func() error {
			disabled, err := repository.DisableMFA(ctx, user, &codes[4])
			if err == nil && !disabled {
				return errors.New("not disabled")
			}
			return err
		}},
		{"PruneMFA", func() error { _, err := repository.PruneMFA(ctx, 10); return err }},
		{"Attempt", func() error {
			_, err := throttler.Attempt(ctx, auth.LoginThrottleKey{HasSource: true, HasPair: true})
			return err
		}},
		{"Attempt with expired counters", func() error {
			if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO forge_login_throttle (scope, key_hash, attempt_count, reset_at) VALUES (1, ?, 1, '2000-01-01 00:00:00.000000')`, []byte("01234567890123456789012345678901")); err != nil {
				return err
			}
			_, err := throttler.Attempt(ctx, auth.LoginThrottleKey{HasSource: true, HasPair: true})
			return err
		}},
		{"ProvisionOwner", func() error {
			for _, table := range []string{"forge_membership_roles", "forge_roles", "forge_sessions", "forge_mfa_challenges", "forge_memberships", "forge_tenants"} {
				if _, err := db.ExecContext(ctx, `DELETE FROM `+table); err != nil {
					return err
				}
			}
			_, err := repository.ProvisionOwner(ctx, authsqlite.OwnerAccount{Organization: "Acme", TenantSlug: "acme", Email: "owner@example.com", Password: "correct horse battery staple"})
			return err
		}},
	}
	for _, step := range steps {
		everyFailure(t, f, step.name, step.op)
	}
}

// TestCorruptStoreIsAnError: values no Forge code writes (a malformed
// permission, a non-UUIDv7 tenant, a short digest) are reported, never
// trusted. CHECK constraints are switched off to plant them.
func TestCorruptStoreIsAnError(t *testing.T) {
	t.Parallel()
	repository, _, _, db := faultyStore(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`PRAGMA ignore_check_constraints = 1`)
	slugs := 0
	signedIn := func(tenant string, permission string) (uuid.UUID, auth.Token) {
		slugs++
		org, user, membership, role := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
		email := tenant + "@example.com"
		exec(`INSERT INTO forge_organizations (id, name) VALUES (?, 'Org')`, org)
		exec(`INSERT INTO forge_tenants (id, organization_id, slug) VALUES (?, ?, ?)`, tenant, org, "tenant-"+string(rune('a'+slugs)))
		exec(`INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES (?, ?, ?, 'h')`, user, email, email)
		exec(`INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES (?, ?, ?)`, membership, user, tenant)
		if permission != "" {
			exec(`INSERT INTO forge_roles (id, tenant_id, name) VALUES (?, ?, 'r')`, role, tenant)
			exec(`INSERT INTO forge_permissions (name) VALUES (?)`, permission)
			exec(`INSERT INTO forge_role_permissions (role_id, permission) VALUES (?, ?)`, role, permission)
			exec(`INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES (?, ?, ?)`, membership, role, tenant)
		}
		token, err := repository.CreateSession(ctx, membership, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return user, token
	}
	_, badPermission := signedIn(uuid.MustNew().String(), "NOT A PERMISSION")
	if _, _, err := repository.ResolveSession(ctx, badPermission.Digest()); err == nil {
		t.Fatal("a malformed permission resolved")
	}
	_, badTenant := signedIn("0d8a5b54-6d3b-4f43-9f5c-2d1f0b2a9e11", "") // a UUIDv4
	if _, _, err := repository.ResolveSession(ctx, badTenant.Digest()); err == nil {
		t.Fatal("a non-UUIDv7 tenant resolved")
	}
	user, _ := signedIn(uuid.MustNew().String(), "")
	exec(`INSERT INTO forge_totp_factors (user_id, secret_digest, secret_ciphertext) VALUES (?, X'0102', X'01')`, user)
	exec(`INSERT INTO forge_totp_enrollments (user_id, secret_digest, secret_ciphertext, expires_at) VALUES (?, X'0102', X'01', '2999-01-01 00:00:00.000000')`, user)
	if _, _, err := repository.LoadActiveTOTP(ctx, user); err == nil {
		t.Fatal("a short factor digest loaded")
	}
	if _, _, err := repository.LoadPendingTOTP(ctx, user); err == nil {
		t.Fatal("a short pending digest loaded")
	}
}
