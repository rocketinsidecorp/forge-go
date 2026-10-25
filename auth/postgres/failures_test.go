package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// identityTables is every table of the identity migrations. The failure
// tests break them one at a time (rename it away) and call an operation:
// whatever statement of the operation needs the table fails, and a failed
// operation must report the error and leave no partial change behind.
var identityTables = []string{
	"forge_users", "forge_organizations", "forge_tenants", "forge_memberships",
	"forge_roles", "forge_permissions", "forge_role_permissions", "forge_membership_roles",
	"forge_sessions", "forge_login_throttle", "forge_password_recovery",
	"forge_totp_enrollments", "forge_totp_factors", "forge_mfa_challenges",
	"forge_mfa_backup_codes", "forge_security_audit_events",
}

func digestOf(label string) auth.Digest { return sha256.Sum256([]byte(label)) }

func backupDigests(prefix string) []auth.Digest {
	digests := make([]auth.Digest, auth.DefaultMFABackupCodeCount)
	for i := range digests {
		digests[i] = digestOf(fmt.Sprintf("%s-%d", prefix, i))
	}
	return digests
}

// userFixture is one account of the failure world.
type userFixture struct {
	user, org, tenant, membership uuid.UUID
	email, slug                   string
	version                       int64
	pending                       auth.Digest
	factor                        auth.Digest
	backup                        []auth.Digest
	challenge                     auth.Digest
	session                       auth.Token
	recoveryA, recoveryB          auth.Digest
}

// failureWorld holds two accounts: a is enrolling (a pending key, no factor)
// and b is enrolled (a factor, backup codes, a pending rotation, a session, a
// sign-in challenge and two recovery links).
type failureWorld struct {
	t    *testing.T
	db   *postgres.DB
	repo *authpostgres.Repository
	a, b userFixture
}

const (
	failureOldHash = "old-hash"
	failureNewHash = "new-hash"
)

func newFailureWorld(t *testing.T) *failureWorld {
	t.Helper()
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	w := &failureWorld{t: t, db: db, repo: repo}
	w.reset()
	return w
}

// reset empties every identity table and seeds the two accounts.
func (w *failureWorld) reset() {
	w.t.Helper()
	ctx := context.Background()
	var truncated []string
	for _, table := range identityTables {
		if table != "forge_permissions" { // seeded by the migrations
			truncated = append(truncated, table)
		}
	}
	if _, err := w.db.Exec(ctx, "TRUNCATE "+strings.Join(truncated, ", ")+" CASCADE"); err != nil {
		w.t.Fatal(err)
	}
	w.a = userFixture{
		user: uuid.MustNew(), org: uuid.MustNew(), tenant: uuid.MustNew(), membership: uuid.MustNew(),
		email: "a@example.com", slug: "alpha", pending: digestOf("a-pending"),
	}
	w.b = userFixture{
		user: uuid.MustNew(), org: uuid.MustNew(), tenant: uuid.MustNew(), membership: uuid.MustNew(),
		email: "b@example.com", slug: "beta", pending: digestOf("b-rotation"), factor: digestOf("b-factor"),
		backup: backupDigests("b-backup"), challenge: digestOf("b-challenge"),
		recoveryA: digestOf("b-recovery-a"), recoveryB: digestOf("b-recovery-b"),
	}
	for _, f := range []*userFixture{&w.a, &w.b} {
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, $2, $2, $3)", []any{f.user, f.email, failureOldHash}},
			{"INSERT INTO forge_organizations (id, name) VALUES ($1, $2)", []any{f.org, f.slug}},
			{"INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, $3)", []any{f.tenant, f.org, f.slug}},
			{"INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", []any{f.membership, f.user, f.tenant}},
		} {
			if _, err := w.db.Exec(ctx, statement.sql, statement.args...); err != nil {
				w.t.Fatal(err)
			}
		}
	}
	soon := time.Now().Add(10 * time.Minute)
	must := func(err error) {
		w.t.Helper()
		if err != nil {
			w.t.Fatal(err)
		}
	}
	must(w.repo.BeginTOTPEnrollment(ctx, w.a.user, w.a.pending, []byte("ciphertext"), soon))
	must(w.repo.BeginTOTPEnrollment(ctx, w.b.user, w.b.factor, []byte("ciphertext"), soon))
	confirmed, err := w.repo.ConfirmTOTPEnrollment(ctx, w.b.user, w.b.factor, 1, w.b.backup)
	if err != nil || !confirmed {
		w.t.Fatalf("seed confirm: %v %v", confirmed, err)
	}
	must(w.repo.BeginTOTPRotation(ctx, w.b.user, w.b.pending, []byte("ciphertext"), soon, nil))
	must(w.db.QueryRow(ctx, "SELECT session_version FROM forge_users WHERE id = $1", w.b.user).Scan(&w.b.version))
	must(w.db.QueryRow(ctx, "SELECT session_version FROM forge_users WHERE id = $1", w.a.user).Scan(&w.a.version))
	w.b.session, err = w.repo.CreateSession(ctx, w.b.membership, time.Now().Add(time.Hour))
	must(err)
	must(w.repo.CreateMFAChallenge(ctx, w.b.challenge, w.b.membership, w.b.version, soon, time.Now().Add(time.Hour)))
	must(w.repo.CreateRecovery(ctx, w.b.user, w.b.recoveryA, time.Now().Add(30*time.Minute)))
	// A second link for the same user replaces the first, so recoveryB is the
	// live one and recoveryA is an invalidated link.
	must(w.repo.CreateRecovery(ctx, w.b.user, w.b.recoveryB, time.Now().Add(30*time.Minute)))
}

// snapshot is the content of every identity table, as text.
func (w *failureWorld) snapshot() string {
	w.t.Helper()
	queries := make([]string, len(identityTables))
	for i, table := range identityTables {
		queries[i] = fmt.Sprintf("SELECT '%[1]s' AS name, coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text), '[]'::jsonb)::text AS rows FROM %[1]s t", table)
	}
	rows, err := w.db.Query(context.Background(), strings.Join(queries, " UNION ALL ")+" ORDER BY name")
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out strings.Builder
	for rows.Next() {
		var name, content string
		if err := rows.Scan(&name, &content); err != nil {
			w.t.Fatal(err)
		}
		out.WriteString(name + "=" + content + "\n")
	}
	if err := rows.Err(); err != nil {
		w.t.Fatal(err)
	}
	return out.String()
}

func (w *failureWorld) rename(table, to string) {
	w.t.Helper()
	if _, err := w.db.Exec(context.Background(), "ALTER TABLE "+table+" RENAME TO "+to); err != nil {
		w.t.Fatal(err)
	}
}

type failureOperation struct {
	name string
	// partial is true for an operation that runs several independent
	// statements and may leave the earlier ones applied when a later one
	// fails (pruning).
	partial bool
	run     func(ctx context.Context, w *failureWorld) error
}

func failureOperations() []failureOperation {
	soon := func() time.Time { return time.Now().Add(10 * time.Minute) }
	return []failureOperation{
		{name: "LookupPassword", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.LookupPassword(ctx, w.b.email, w.b.slug)
			return err
		}},
		{name: "ReplacePasswordHash", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ReplacePasswordHash(ctx, w.b.user, failureOldHash, failureNewHash)
			return err
		}},
		{name: "CreateSession", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.CreateSession(ctx, w.b.membership, soon())
			return err
		}},
		{name: "CreateSessionAtVersion", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.CreateSessionAtVersion(ctx, w.b.membership, w.b.version, soon())
			return err
		}},
		{name: "RevokeSession", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.RevokeSession(ctx, w.b.session)
		}},
		{name: "RevokeUserSessions", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.RevokeUserSessions(ctx, w.b.user)
		}},
		{name: "ResolveSession", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.ResolveSession(ctx, w.b.session.Digest())
			return err
		}},
		{name: "RecordSecurityEvent", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.RecordSecurityEvent(ctx, auth.SecurityEvent{
				Kind: auth.SecurityLoginSucceeded, Outcome: auth.SecurityOutcomeSucceeded,
				SubjectID: w.b.user, MembershipID: w.b.membership,
			})
		}},
		{name: "RecordSecurityEvent with a time", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.RecordSecurityEvent(ctx, auth.SecurityEvent{
				Kind: auth.SecurityLoginSucceeded, Outcome: auth.SecurityOutcomeSucceeded,
				SubjectID: w.b.user, OccurredAt: time.Now(),
			})
		}},
		{name: "LookupRecovery", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.LookupRecovery(ctx, w.b.email, w.b.slug)
			return err
		}},
		{name: "RecoveryTokenUsable", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.RecoveryTokenUsable(ctx, w.b.recoveryB)
			return err
		}},
		{name: "CreateRecovery", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.CreateRecovery(ctx, w.a.user, digestOf("new-recovery"), time.Now().Add(30*time.Minute))
		}},
		{name: "InvalidateRecovery", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.InvalidateRecovery(ctx, w.b.recoveryB)
		}},
		{name: "ConsumeRecovery", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ConsumeRecovery(ctx, w.b.recoveryB, failureNewHash)
			return err
		}},
		{name: "PruneRecovery", partial: true, run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.PruneRecovery(ctx, 10)
			return err
		}},
		{name: "BeginTOTPEnrollment", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.BeginTOTPEnrollment(ctx, w.a.user, digestOf("again"), []byte("ciphertext"), soon())
		}},
		{name: "LoadPendingTOTP", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.LoadPendingTOTP(ctx, w.a.user)
			return err
		}},
		{name: "ConfirmTOTPEnrollment", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ConfirmTOTPEnrollment(ctx, w.a.user, w.a.pending, 5, backupDigests("a-new"))
			return err
		}},
		{name: "CreateMFAChallenge", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.CreateMFAChallenge(ctx, digestOf("fresh-challenge"), w.b.membership, w.b.version, soon(), time.Now().Add(time.Hour))
		}},
		{name: "LoadMFAChallenge", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.LoadMFAChallenge(ctx, w.b.challenge)
			return err
		}},
		{name: "ConsumeMFAChallenge", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.ConsumeMFAChallenge(ctx, w.b.challenge, w.b.factor, 5)
			return err
		}},
		{name: "ConsumeMFABackupChallenge", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.ConsumeMFABackupChallenge(ctx, w.b.challenge, w.b.backup[0])
			return err
		}},
		{name: "BeginTOTPRotation", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.BeginTOTPRotation(ctx, w.b.user, digestOf("rotate-again"), []byte("ciphertext"), soon(), nil)
		}},
		{name: "BeginTOTPRotation with a step-up code", run: func(ctx context.Context, w *failureWorld) error {
			return w.repo.BeginTOTPRotation(ctx, w.b.user, digestOf("rotate-again"), []byte("ciphertext"), soon(), &w.b.backup[1])
		}},
		{name: "ConfirmTOTPRotation", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ConfirmTOTPRotation(ctx, w.b.user, w.b.pending, 9, backupDigests("b-new"), nil)
			return err
		}},
		{name: "ConfirmTOTPRotation with a step-up code", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ConfirmTOTPRotation(ctx, w.b.user, w.b.pending, 9, backupDigests("b-new"), &w.b.backup[1])
			return err
		}},
		{name: "DisableMFA", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.DisableMFA(ctx, w.b.user, nil)
			return err
		}},
		{name: "DisableMFA with a step-up code", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.DisableMFA(ctx, w.b.user, &w.b.backup[1])
			return err
		}},
		{name: "PruneMFA", partial: true, run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.PruneMFA(ctx, 10)
			return err
		}},
		{name: "LookupSubjectPassword", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.LookupSubjectPassword(ctx, w.b.user)
			return err
		}},
		{name: "LoadMFAStatus", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.LoadMFAStatus(ctx, w.b.user)
			return err
		}},
		{name: "LoadActiveTOTP", run: func(ctx context.Context, w *failureWorld) error {
			_, _, err := w.repo.LoadActiveTOTP(ctx, w.b.user)
			return err
		}},
		{name: "AdvanceTOTPCounter", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.AdvanceTOTPCounter(ctx, w.b.user, w.b.factor, 9)
			return err
		}},
		{name: "ConsumeMFABackupCode", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ConsumeMFABackupCode(ctx, w.b.user, w.b.backup[2])
			return err
		}},
		{name: "HasMFABackupCode", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.HasMFABackupCode(ctx, w.b.user, w.b.backup[3])
			return err
		}},
		{name: "ReplaceMFABackupCodes", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ReplaceMFABackupCodes(ctx, w.b.user, backupDigests("b-replaced"), nil)
			return err
		}},
		{name: "ReplaceMFABackupCodes with a step-up code", run: func(ctx context.Context, w *failureWorld) error {
			_, err := w.repo.ReplaceMFABackupCodes(ctx, w.b.user, backupDigests("b-replaced"), &w.b.backup[1])
			return err
		}},
	}
}

// TestIdentityOperationsFailClosedWhenATableIsUnavailable breaks each
// identity table in turn under each repository operation. An operation that
// needs the table must return an error, and it must leave every table as it
// was (the operations that run several statements are transactions).
func TestIdentityOperationsFailClosedWhenATableIsUnavailable(t *testing.T) {
	for _, op := range failureOperations() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			w := newFailureWorld(t)
			ctx := context.Background()
			if err := op.run(ctx, w); err != nil {
				t.Fatalf("the operation fails on a healthy database: %v", err)
			}
			failed := 0
			dirty := false // a succeeded or partial operation changed the world
			for _, table := range identityTables {
				if dirty {
					w.reset()
					dirty = false
				}
				before := w.snapshot()
				w.rename(table, table+"_gone")
				err := op.run(ctx, w)
				w.rename(table+"_gone", table)
				if err == nil {
					dirty = true // the operation does not use this table
					continue
				}
				failed++
				if op.partial {
					dirty = true
					continue
				}
				if after := w.snapshot(); after != before {
					t.Errorf("%s unavailable: the operation failed (%v) but changed the database", table, err)
					dirty = true
				}
			}
			if failed == 0 {
				t.Error("no table made the operation fail: the harness is not exercising it")
			}
		})
	}
}

func TestClosedPoolFailsEveryOperation(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	w.db.Close()
	for _, op := range failureOperations() {
		if err := op.run(context.Background(), w); err == nil {
			t.Errorf("%s succeeded on a closed pool", op.name)
		}
	}
}

func TestIdentityRepositoryRefusesMalformedInput(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	soon, past := time.Now().Add(10*time.Minute), time.Now().Add(-time.Minute)
	digest := digestOf("x")
	before := w.snapshot()
	cases := map[string]error{}
	record := func(name string, err error) { cases[name] = err }

	record("enroll non-v7 subject", w.repo.BeginTOTPEnrollment(ctx, uuid.UUID{}, digest, []byte("c"), soon))
	record("enroll no ciphertext", w.repo.BeginTOTPEnrollment(ctx, w.a.user, digest, nil, soon))
	record("enroll expired", w.repo.BeginTOTPEnrollment(ctx, w.a.user, digest, []byte("c"), past))
	record("rotate non-v7 subject", w.repo.BeginTOTPRotation(ctx, uuid.UUID{}, digest, []byte("c"), soon, nil))
	record("rotate no ciphertext", w.repo.BeginTOTPRotation(ctx, w.b.user, digest, nil, soon, nil))
	record("rotate expired", w.repo.BeginTOTPRotation(ctx, w.b.user, digest, []byte("c"), past, nil))
	record("challenge non-v7 membership", w.repo.CreateMFAChallenge(ctx, digest, uuid.UUID{}, 1, soon, soon))
	record("challenge no version", w.repo.CreateMFAChallenge(ctx, digest, w.b.membership, 0, soon, soon))
	record("challenge expired", w.repo.CreateMFAChallenge(ctx, digest, w.b.membership, 1, past, soon))
	record("challenge session expired", w.repo.CreateMFAChallenge(ctx, digest, w.b.membership, 1, soon, past))
	var err error
	_, err = w.repo.ConfirmTOTPEnrollment(ctx, w.a.user, w.a.pending, -1, w.b.backup)
	record("confirm negative counter", err)
	_, err = w.repo.ConfirmTOTPEnrollment(ctx, w.a.user, w.a.pending, 1, w.b.backup[:3])
	record("confirm short backup set", err)
	_, err = w.repo.ConfirmTOTPRotation(ctx, w.b.user, w.b.pending, -1, w.b.backup, nil)
	record("rotation confirm negative counter", err)
	_, err = w.repo.ConfirmTOTPRotation(ctx, w.b.user, w.b.pending, 1, w.b.backup[:3], nil)
	record("rotation confirm short backup set", err)
	_, _, err = w.repo.ConsumeMFAChallenge(ctx, w.b.challenge, w.b.factor, -1)
	record("consume negative counter", err)
	_, err = w.repo.AdvanceTOTPCounter(ctx, w.b.user, w.b.factor, -1)
	record("advance negative counter", err)
	_, err = w.repo.DisableMFA(ctx, uuid.UUID{}, nil)
	record("disable non-v7 subject", err)
	_, err = w.repo.ReplaceMFABackupCodes(ctx, uuid.UUID{}, w.b.backup, nil)
	record("replace non-v7 subject", err)
	_, _, err = w.repo.LookupSubjectPassword(ctx, uuid.UUID{})
	record("subject password non-v7", err)
	_, err = w.repo.PruneMFA(ctx, 0)
	record("prune MFA limit", err)
	_, err = w.repo.PruneRecovery(ctx, 0)
	record("prune recovery limit", err)
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Nothing was changed by the refusals.
	if after := w.snapshot(); after != before {
		t.Error("a refused call changed the database")
	}
}

func TestLoginThrottlerFailsClosedWhenItsTableIsUnavailable(t *testing.T) {
	t.Parallel()
	if _, err := authpostgres.NewLoginThrottler(nil, auth.DefaultLoginThrottleConfig()); err == nil {
		t.Error("a nil database was accepted")
	}
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	if _, err := authpostgres.NewLoginThrottler(db, auth.LoginThrottleConfig{}); err == nil {
		t.Error("an empty config was accepted")
	}
	recovery, err := authpostgres.NewRecoveryThrottler(db)
	if err != nil || recovery == nil {
		t.Fatalf("recovery throttler: %v", err)
	}
	throttler, err := authpostgres.NewLoginThrottler(db, auth.DefaultLoginThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := throttleKey("alice@example.com", "192.0.2.1", true)
	if _, err := throttler.PruneExpired(ctx, 0); err == nil {
		t.Error("a prune limit of 0 was accepted")
	}
	if _, err := db.Exec(ctx, "ALTER TABLE forge_login_throttle RENAME TO forge_login_throttle_gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := throttler.Attempt(ctx, key); err == nil {
		t.Error("Attempt succeeded without its table")
	}
	if err := throttler.Success(ctx, key); err == nil {
		t.Error("Success succeeded without its table")
	}
	if _, err := throttler.PruneExpired(ctx, 5); err == nil {
		t.Error("PruneExpired succeeded without its table")
	}
	// A canceled context is an error, not a decision to allow.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := throttler.Attempt(canceled, key); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Attempt: %v", err)
	}
}

func TestSessionAndPasswordInputsAreValidated(t *testing.T) {
	t.Parallel()
	if _, err := authpostgres.New(nil); err == nil {
		t.Error("a nil database was accepted")
	}
	w := newFailureWorld(t)
	ctx := context.Background()
	soon, past := time.Now().Add(time.Hour), time.Now().Add(-time.Minute)
	before := w.snapshot()
	var err error
	cases := map[string]error{}
	_, err = w.repo.ReplacePasswordHash(ctx, uuid.UUID{}, "a", "b")
	cases["replace non-v7 user"] = err
	_, err = w.repo.ReplacePasswordHash(ctx, w.b.user, "same", "same")
	cases["replace identical hashes"] = err
	_, err = w.repo.ReplacePasswordHash(ctx, w.b.user, "", "b")
	cases["replace empty previous"] = err
	_, err = w.repo.CreateSession(ctx, uuid.UUID{}, soon)
	cases["session non-v7 membership"] = err
	_, err = w.repo.CreateSession(ctx, w.b.membership, past)
	cases["session expired"] = err
	_, err = w.repo.CreateSessionAtVersion(ctx, uuid.UUID{}, 1, soon)
	cases["versioned session non-v7 membership"] = err
	_, err = w.repo.CreateSessionAtVersion(ctx, w.b.membership, 0, soon)
	cases["versioned session no version"] = err
	_, err = w.repo.CreateSessionAtVersion(ctx, w.b.membership, 1, past)
	cases["versioned session expired"] = err
	cases["revoke sessions of a non-v7 user"] = w.repo.RevokeUserSessions(ctx, uuid.UUID{})
	cases["revoke sessions of an unknown user"] = w.repo.RevokeUserSessions(ctx, uuid.MustNew())
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if !errors.Is(cases["session expired"], authpostgres.ErrExpiryRequired) {
		t.Errorf("session expired: %v", cases["session expired"])
	}
	if after := w.snapshot(); after != before {
		t.Error("a refused call changed the database")
	}
}

// TestProvisionOwnerFailsClosedWhenATableIsUnavailable: setup is one
// transaction, so a failing statement leaves an empty identity database.
func TestProvisionOwnerFailsClosedWhenATableIsUnavailable(t *testing.T) {
	account := authpostgres.OwnerAccount{Organization: "Acme", TenantSlug: "acme", Email: "owner@example.com", Password: "correct horse battery staple"}
	for _, table := range []string{
		"forge_organizations", "forge_tenants", "forge_users", "forge_memberships", "forge_roles",
		"forge_membership_roles", "forge_permissions", "forge_role_permissions", "forge_security_audit_events",
	} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			db := postgrestest.NewMigrated(t, authpostgres.Migrations())
			repo, err := authpostgres.New(db)
			if err != nil {
				t.Fatal(err)
			}
			w := &failureWorld{t: t, db: db, repo: repo}
			ctx := context.Background()
			if _, err := db.Exec(ctx, "INSERT INTO forge_permissions (name) VALUES ('tasks:read') ON CONFLICT DO NOTHING"); err != nil {
				t.Fatal(err)
			}
			before := w.snapshot()
			w.rename(table, table+"_gone")
			_, err = repo.ProvisionOwner(ctx, account)
			w.rename(table+"_gone", table)
			if err == nil {
				t.Fatalf("setup succeeded without %s", table)
			}
			if after := w.snapshot(); after != before {
				t.Errorf("setup failed (%v) but changed the database", err)
			}
		})
	}
}

func TestSetupCommandRefusesABadOrganizationAndAnUnusableDatabase(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("not a database url")
	run := func(args ...string) error {
		return authpostgres.SetupCommand().Run(context.Background(), forge.CommandEnv{
			Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: io.Discard, Stderr: io.Discard,
			Stdin:     strings.NewReader("correct horse battery staple\n"),
			LookupEnv: func(string) (string, bool) { return "", false },
		}, args)
	}
	err := run("--tenant", "acme", "--email", "owner@example.com", "--organization", "bad\x07name", "--password-stdin")
	if !errors.Is(err, forge.ErrUsage) || !strings.Contains(err.Error(), "--organization") {
		t.Errorf("bad organization: %v", err)
	}
	if err := run("--tenant", "acme", "--email", "owner@example.com", "--password-stdin"); err == nil || errors.Is(err, forge.ErrUsage) {
		t.Errorf("unusable database: %v", err)
	}
	if err := run("--nope"); !errors.Is(err, forge.ErrUsage) {
		t.Errorf("unknown flag: %v", err)
	}
}
