package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// notUUIDv7 is a version 4 UUID: well formed, but not an identifier Forge
// writes.
func notUUIDv7() uuid.UUID {
	id := uuid.MustNew()
	id[6] = 0x40 | id[6]&0x0f
	return id
}

func TestNewRefusesNilAndFailingOptions(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	if repo, err := authpostgres.New(db, nil); err == nil || repo != nil {
		t.Errorf("a nil option: %v, %v", repo, err)
	}
	if repo, err := authpostgres.New(db, authpostgres.WithSessionIdleTimeout(time.Second)); err == nil || repo != nil {
		t.Errorf("an option that fails: %v, %v", repo, err)
	}
}

func TestRecoveryRefusesInputItCannotStore(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	soon := time.Now().Add(10 * time.Minute)
	if err := w.repo.CreateRecovery(ctx, notUUIDv7(), digestOf("r"), soon); err == nil || !strings.Contains(err.Error(), "UUIDv7") {
		t.Errorf("a user ID that is not a UUIDv7: %v", err)
	}
	if err := w.repo.CreateRecovery(ctx, w.a.user, digestOf("r"), time.Now().Add(-time.Minute)); err == nil || !strings.Contains(err.Error(), "future") {
		t.Errorf("an expired link: %v", err)
	}
	if err := w.repo.CreateRecovery(ctx, uuid.MustNew(), digestOf("r"), soon); !errors.Is(err, authpostgres.ErrRecoveryIdentityRequired) {
		t.Errorf("an unknown user: %v", err)
	}
	if usable, err := w.repo.RecoveryTokenUsable(ctx, digestOf("r")); err != nil || usable {
		t.Errorf("a refused link is usable: %v, %v", usable, err)
	}
	before := w.snapshot()
	if consumed, err := w.repo.ConsumeRecovery(ctx, w.b.recoveryB, ""); err == nil || consumed {
		t.Errorf("a link consumed with no new password hash: %v, %v", consumed, err)
	}
	if w.snapshot() != before {
		t.Error("a refused ConsumeRecovery changed the database")
	}
}

// Every MFA operation on an account that does not exist reports it, or
// reports that nothing matched, and writes nothing.
func TestMFAOperationsRefuseUnknownAccounts(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	soon := time.Now().Add(10 * time.Minute)
	unknown, ghost := uuid.MustNew(), uuid.MustNew()
	before := w.snapshot()

	if err := w.repo.BeginTOTPEnrollment(ctx, unknown, digestOf("x"), []byte("ciphertext"), soon); !errors.Is(err, authpostgres.ErrMFAIdentityRequired) {
		t.Errorf("BeginTOTPEnrollment: %v", err)
	}
	if _, found, err := w.repo.LoadPendingTOTP(ctx, unknown); err != nil || found {
		t.Errorf("LoadPendingTOTP: %v, %v", found, err)
	}
	if confirmed, err := w.repo.ConfirmTOTPEnrollment(ctx, unknown, digestOf("x"), 1, backupDigests("x")); err != nil || confirmed {
		t.Errorf("ConfirmTOTPEnrollment: %v, %v", confirmed, err)
	}
	if err := w.repo.CreateMFAChallenge(ctx, digestOf("x"), ghost, 1, soon, time.Now().Add(time.Hour)); !errors.Is(err, authpostgres.ErrMFAIdentityRequired) {
		t.Errorf("CreateMFAChallenge for an unknown membership: %v", err)
	}
	// An account that is only enrolling has no factor to rotate.
	for name, subject := range map[string]uuid.UUID{"unknown": unknown, "not enrolled": w.a.user} {
		if err := w.repo.BeginTOTPRotation(ctx, subject, digestOf("x"), []byte("ciphertext"), soon, nil); !errors.Is(err, auth.ErrMFANotEnabled) {
			t.Errorf("BeginTOTPRotation (%s): %v", name, err)
		}
	}
	if w.snapshot() != before {
		t.Error("a refused operation changed the database")
	}
}

// A code that was already used, or an older one, does not complete the
// sign-in challenge, and the challenge stays usable with a fresh code.
func TestReplayedTOTPCodeDoesNotConsumeTheChallenge(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	// The seeded factor accepted counter 1.
	for _, counter := range []int64{0, 1} {
		if _, consumed, err := w.repo.ConsumeMFAChallenge(ctx, w.b.challenge, w.b.factor, counter); err != nil || consumed {
			t.Fatalf("counter %d: consumed=%v err=%v", counter, consumed, err)
		}
	}
	if _, found, err := w.repo.LoadMFAChallenge(ctx, w.b.challenge); err != nil || !found {
		t.Fatalf("the challenge was consumed by a replayed code: found=%v err=%v", found, err)
	}
	membership, consumed, err := w.repo.ConsumeMFAChallenge(ctx, w.b.challenge, w.b.factor, 2)
	if err != nil || !consumed || membership != w.b.membership {
		t.Fatalf("a fresh code: %v, %v, %v", membership, consumed, err)
	}
}

// dropDigestChecks removes the length checks of the digest columns of tables,
// to put the identity store in a state its constraints normally forbid (a
// restore from a damaged backup, a manual edit).
func dropDigestChecks(t *testing.T, db *postgres.DB, tables ...string) {
	t.Helper()
	list := "'" + strings.Join(tables, "', '") + "'"
	if _, err := db.Exec(context.Background(), `
		DO $$
		DECLARE c record;
		BEGIN
			FOR c IN SELECT conrelid::regclass AS relation, conname FROM pg_constraint
			         WHERE contype = 'c' AND conrelid::regclass::text IN (`+list+`)
			           AND pg_get_constraintdef(oid) LIKE '%octet_length%digest%'
			LOOP
				EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', c.relation, c.conname);
			END LOOP;
		END $$`); err != nil {
		t.Fatal(err)
	}
}

// A stored digest of the wrong length is data the store never writes. Reading
// it is an error that says so, never a zero-padded digest that could match.
func TestStoredDigestsOfTheWrongLengthFailClosed(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	dropDigestChecks(t, w.db, "forge_totp_enrollments", "forge_totp_factors", "forge_mfa_challenges")
	short := []byte{1, 2, 3}
	for _, statement := range []string{
		`UPDATE forge_totp_enrollments SET secret_digest = $1 WHERE user_id = $2`,
		`UPDATE forge_totp_factors SET secret_digest = $1 WHERE user_id = $2`,
		`UPDATE forge_mfa_challenges SET factor_secret_digest = $1 WHERE factor_secret_digest = (SELECT secret_digest FROM forge_totp_factors WHERE user_id = $2)`,
	} {
		if _, err := w.db.Exec(ctx, statement, short, w.b.user); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The challenge update above ran after the factor's, so point it at the
	// shortened digest explicitly.
	if _, err := w.db.Exec(ctx, `UPDATE forge_mfa_challenges SET factor_secret_digest = $1`, short); err != nil {
		t.Fatal(err)
	}
	if _, found, err := w.repo.LoadPendingTOTP(ctx, w.b.user); err == nil || found || !strings.Contains(err.Error(), "invalid digest") {
		t.Errorf("LoadPendingTOTP: found=%v err=%v", found, err)
	}
	if _, found, err := w.repo.LoadActiveTOTP(ctx, w.b.user); err == nil || found || !strings.Contains(err.Error(), "invalid digest") {
		t.Errorf("LoadActiveTOTP: found=%v err=%v", found, err)
	}
	if _, found, err := w.repo.LoadMFAChallenge(ctx, w.b.challenge); err == nil || found || !strings.Contains(err.Error(), "invalid digest") {
		t.Errorf("LoadMFAChallenge: found=%v err=%v", found, err)
	}
}

// A session whose tenant, user or permission is not something Forge writes
// does not resolve, and no principal is built. An identifier that is not a
// UUIDv7 is already refused when it is read (uuid.UUID.Scan), as an internal
// error that does not repeat it; a malformed permission is refused by name.
func TestResolveSessionRefusesDataTheApplicationNeverWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tt := range map[string]struct {
		user, tenant uuid.UUID
		permission   string
		want         string
	}{
		"a tenant that is not a UUIDv7": {uuid.MustNew(), notUUIDv7(), "", "internal_error"},
		"a user that is not a UUIDv7":   {notUUIDv7(), uuid.MustNew(), "", "internal_error"},
		"a malformed permission":        {uuid.MustNew(), uuid.MustNew(), "Not A Permission", "invalid permission"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := postgrestest.NewMigrated(t, authpostgres.Migrations())
			repo, err := authpostgres.New(db)
			if err != nil {
				t.Fatal(err)
			}
			membership := uuid.MustNew()
			insertIdentityFixture(t, db, tt.user, uuid.MustNew(), tt.tenant, membership, "odd@example.com", "odd@example.com", "Odd", "odd")
			if tt.permission != "" {
				role := uuid.MustNew()
				for _, statement := range []struct {
					sql  string
					args []any
				}{
					{`ALTER TABLE forge_permissions DROP CONSTRAINT forge_permissions_name_check`, nil},
					{`INSERT INTO forge_permissions (name) VALUES ($1)`, []any{tt.permission}},
					{`INSERT INTO forge_roles (id, tenant_id, name) VALUES ($1, $2, 'odd')`, []any{role, tt.tenant}},
					{`INSERT INTO forge_role_permissions (role_id, permission) VALUES ($1, $2)`, []any{role, tt.permission}},
					{`INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES ($1, $2, $3)`, []any{membership, role, tt.tenant}},
				} {
					if _, err := db.Exec(ctx, statement.sql, statement.args...); err != nil {
						t.Fatalf("%s: %v", statement.sql, err)
					}
				}
			}
			token, err := repo.CreateSession(ctx, membership, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			session, found, err := repo.ResolveSession(ctx, token.Digest())
			if err == nil || found || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), tt.tenant.String()) || session.Principal.SubjectID() != (uuid.UUID{}) {
				t.Errorf("ResolveSession = %+v, %v, %v; want an error saying %q", session, found, err, tt.want)
			}
		})
	}
}

// Recording that a session was used is part of resolving it: when the write
// is refused the lookup fails rather than reporting a session whose idle
// clock was not kept, and the session is intact afterwards.
func TestSessionTouchFailureFailsTheLookup(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	if _, err := w.db.Exec(ctx, `UPDATE forge_sessions SET last_seen_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE FUNCTION forge_test_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'write refused by the test' USING ERRCODE = 'XX000'; END $$`,
		`CREATE TRIGGER forge_test_refuse BEFORE UPDATE ON forge_sessions FOR EACH STATEMENT EXECUTE FUNCTION forge_test_refuse()`,
	} {
		if _, err := w.db.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if session, found, err := w.repo.ResolveSession(ctx, w.b.session.Digest()); err == nil || found || session.Principal.SubjectID() != (uuid.UUID{}) {
		t.Fatalf("ResolveSession with the touch refused = %v, %v", found, err)
	}
	if _, err := w.db.Exec(ctx, `DROP TRIGGER forge_test_refuse ON forge_sessions`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := w.repo.ResolveSession(ctx, w.b.session.Digest()); err != nil || !found {
		t.Fatalf("the session after the refusal: found=%v err=%v", found, err)
	}
}

// An attempt whose counters cannot be updated is an error, not a free pass,
// and it leaves no counter behind.
func TestLoginThrottlerFailsWhenCountersCannotBeUpdated(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	ctx := context.Background()
	throttler, err := authpostgres.NewLoginThrottler(db, auth.DefaultLoginThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE FUNCTION forge_test_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'write refused by the test' USING ERRCODE = 'XX000'; END $$`,
		`CREATE TRIGGER forge_test_refuse BEFORE UPDATE ON forge_login_throttle FOR EACH STATEMENT EXECUTE FUNCTION forge_test_refuse()`,
	} {
		if _, err := db.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	key := auth.LoginThrottleKey{Account: sha256.Sum256([]byte("account"))}
	if decision, err := throttler.Attempt(ctx, key); err == nil || decision.Allowed {
		t.Fatalf("Attempt with the update refused = %+v, %v", decision, err)
	}
	var counters int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM forge_login_throttle`).Scan(&counters); err != nil || counters != 0 {
		t.Errorf("the failed attempt left %d counters (%v)", counters, err)
	}
}

// The password of the setup command can come from standard input; the ways
// that can fail are reported before anything is created.
func TestSetupCommandRefusesAnUnreadablePasswordFromStandardInput(t *testing.T) {
	t.Parallel()
	run := func(stdin io.Reader) (string, error) {
		var stdout bytes.Buffer
		err := authpostgres.SetupCommand().Run(context.Background(), forge.CommandEnv{
			Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Stdout: &stdout, Stderr: io.Discard, Stdin: stdin,
			LookupEnv: func(string) (string, bool) { return "", false },
		}, []string{"--tenant", "acme", "--email", "owner@example.com", "--password-stdin"})
		return stdout.String(), err
	}
	for name, tt := range map[string]struct {
		stdin io.Reader
		want  string
	}{
		"no standard input": {nil, "no standard input"},
		"empty input":       {strings.NewReader(""), "read the password from standard input"},
		"an endless line":   {strings.NewReader(strings.Repeat("x", 1<<20)), "read the password from standard input"},
	} {
		out, err := run(tt.stdin)
		if err == nil || !strings.Contains(err.Error(), tt.want) || out != "" || errors.Is(err, forge.ErrUsage) {
			t.Errorf("%s: out=%q err=%v", name, out, err)
		}
	}
}

func TestLookupRecoveryFindsOnlyActiveMembersOfTheTenant(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	identity, found, err := w.repo.LookupRecovery(ctx, w.b.email, w.b.slug)
	if err != nil || !found || identity.SubjectID != w.b.user || identity.Email != w.b.email {
		t.Fatalf("LookupRecovery = %+v, %v, %v", identity, found, err)
	}
	// The same account in another tenant, or an unknown one, is not found,
	// and the lookup does not say which of the two it was.
	for name, lookup := range map[string][2]string{
		"another tenant":  {w.b.email, w.a.slug},
		"an unknown user": {"nobody@example.com", w.b.slug},
	} {
		if identity, found, err := w.repo.LookupRecovery(ctx, lookup[0], lookup[1]); err != nil || found || identity.SubjectID != (uuid.UUID{}) {
			t.Errorf("%s: %+v, %v, %v", name, identity, found, err)
		}
	}
}

// A challenge is only issued for the credentials the password check saw: if
// the account's credential version moved on, nothing is stored and the
// challenge the account already holds is kept.
func TestMFAChallengeIsRefusedForAStaleCredentialVersion(t *testing.T) {
	t.Parallel()
	w := newFailureWorld(t)
	ctx := context.Background()
	before := w.snapshot()
	err := w.repo.CreateMFAChallenge(ctx, digestOf("stale"), w.b.membership, w.b.version+1, time.Now().Add(10*time.Minute), time.Now().Add(time.Hour))
	if !errors.Is(err, auth.ErrCredentialsInvalid) {
		t.Fatalf("CreateMFAChallenge with a stale version: %v", err)
	}
	if w.snapshot() != before {
		t.Error("a refused challenge changed the database")
	}
	if _, found, err := w.repo.LoadMFAChallenge(ctx, w.b.challenge); err != nil || !found {
		t.Errorf("the existing challenge was consumed: found=%v err=%v", found, err)
	}
}

// While another transaction holds the counters of a key, an attempt that
// cannot lock them within its deadline fails instead of being counted or
// allowed uncounted; once the lock is released the same key is counted again.
func TestLoginThrottlerFailsWhenItCannotLockTheCounters(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	throttler, err := authpostgres.NewLoginThrottler(db, auth.DefaultLoginThrottleConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := auth.LoginThrottleKey{Account: sha256.Sum256([]byte("locked"))}
	if decision, err := throttler.Attempt(ctx, key); err != nil || !decision.Allowed {
		t.Fatalf("first attempt = %+v, %v", decision, err)
	}
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM forge_login_throttle FOR UPDATE`); err != nil {
			return err
		}
		blocked, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if decision, err := throttler.Attempt(blocked, key); err == nil || decision.Allowed {
			t.Errorf("Attempt while the counters are locked = %+v, %v", decision, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := throttler.Attempt(ctx, key); err != nil || !decision.Allowed {
		t.Errorf("attempt after the lock was released = %+v, %v", decision, err)
	}
}
