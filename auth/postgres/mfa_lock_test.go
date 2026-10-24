package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// mfaLockFixture is one active user with a membership, a factor and backup
// codes.
type mfaLockFixture struct {
	db           *postgres.DB
	repo         *authpostgres.Repository
	userID       uuid.UUID
	membershipID uuid.UUID
	factor       auth.Digest
	backup       []auth.Digest
	version      int64
}

func newMFALockFixture(t *testing.T) *mfaLockFixture {
	t.Helper()
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f := &mfaLockFixture{db: db, repo: repo, userID: uuid.MustNew(), membershipID: uuid.MustNew()}
	orgID, tenantID := uuid.MustNew(), uuid.MustNew()
	passwordHash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, 'lock@example.com', 'lock@example.com', $2)", []any{f.userID, passwordHash}},
		{"INSERT INTO forge_organizations (id, name) VALUES ($1, 'Lock')", []any{orgID}},
		{"INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, 'lock')", []any{tenantID, orgID}},
		{"INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", []any{f.membershipID, f.userID, tenantID}},
	} {
		if _, err := db.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(ctx, "SELECT session_version FROM forge_users WHERE id = $1", f.userID).Scan(&f.version); err != nil {
		t.Fatal(err)
	}
	f.enable(t)
	return f
}

// enable installs a fresh factor and backup codes.
func (f *mfaLockFixture) enable(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f.factor = sha256.Sum256([]byte(uuid.MustNew().String()))
	if err := f.repo.BeginTOTPEnrollment(ctx, f.userID, f.factor, []byte("ciphertext"), time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.backup = make([]auth.Digest, auth.DefaultMFABackupCodeCount)
	for i := range f.backup {
		f.backup[i] = sha256.Sum256([]byte(uuid.MustNew().String()))
	}
	confirmed, err := f.repo.ConfirmTOTPEnrollment(ctx, f.userID, f.factor, 1, f.backup)
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%v err=%v", confirmed, err)
	}
	if err := f.db.QueryRow(ctx, "SELECT session_version FROM forge_users WHERE id = $1", f.userID).Scan(&f.version); err != nil {
		t.Fatal(err)
	}
}

// challenge creates a live challenge and returns its digest.
func (f *mfaLockFixture) challenge(t *testing.T) auth.Digest {
	t.Helper()
	digest := sha256.Sum256([]byte(uuid.MustNew().String()))
	if err := f.repo.CreateMFAChallenge(context.Background(), digest, f.membershipID, f.version, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return digest
}

// disableWhileWaiting is a factor change done by hand: it locks the user row
// as DisableMFA does, waits until step (a sign-in step) waits for that row,
// then takes the rows the step would need NOWAIT (55P03 if the waiting step
// held any of them), removes the factor and its backup codes, voids the
// challenges and commits. It returns step's error.
func (f *mfaLockFixture) disableWhileWaiting(t *testing.T, step func() error) error {
	t.Helper()
	ctx := context.Background()
	holder, err := f.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT id FROM forge_users WHERE id = $1 FOR UPDATE", f.userID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- step() }()
	if err := waitForLockWait(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"SELECT 1 FROM forge_memberships WHERE user_id = $1 FOR UPDATE NOWAIT",
		"SELECT 1 FROM forge_totp_factors WHERE user_id = $1 FOR UPDATE NOWAIT",
		"SELECT 1 FROM forge_mfa_backup_codes WHERE user_id = $1 FOR UPDATE NOWAIT",
		`SELECT 1 FROM forge_mfa_challenges c JOIN forge_memberships m ON m.id = c.membership_id
		 WHERE m.user_id = $1 FOR UPDATE OF c NOWAIT`,
		"DELETE FROM forge_mfa_backup_codes WHERE user_id = $1",
		"DELETE FROM forge_totp_factors WHERE user_id = $1",
		`UPDATE forge_mfa_challenges c SET consumed_at = now() FROM forge_memberships m
		 WHERE c.membership_id = m.id AND m.user_id = $1 AND c.consumed_at IS NULL`,
	} {
		if _, err := holder.Exec(ctx, statement, f.userID); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the sign-in step did not finish")
		return nil
	}
}

// waitForLockWait returns once a statement in the test's database waits for
// a lock.
func waitForLockWait(ctx context.Context, db *postgres.DB) error {
	for range 1000 {
		var waiting int
		if err := db.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database()
		`).Scan(&waiting); err != nil {
			return err
		}
		if waiting > 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("no statement waited for the user lock")
}

// TestMFASignInStepsLockTheUserBeforeAnyMFARow: issuing and completing a
// challenge lock the user row before any MFA row, so while a factor change
// holds the user row they wait holding nothing; the change takes the
// membership, factor, backup code and challenge rows without waiting, and
// the step then refuses cleanly instead of deadlocking (40P01). Before,
// issuing locked the membership first and completing locked the challenge
// first, then waited for the user row.
func TestMFASignInStepsLockTheUserBeforeAnyMFARow(t *testing.T) {
	f := newMFALockFixture(t)
	ctx := context.Background()

	issued := sha256.Sum256([]byte("issued"))
	err := f.disableWhileWaiting(t, func() error {
		return f.repo.CreateMFAChallenge(ctx, issued, f.membershipID, f.version, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour))
	})
	if !errors.Is(err, authpostgres.ErrMFAIdentityRequired) {
		t.Fatalf("issuing during a disable: %v", err)
	}

	f.enable(t)
	live := f.challenge(t)
	err = f.disableWhileWaiting(t, func() error {
		_, consumed, err := f.repo.ConsumeMFAChallenge(ctx, live, f.factor, 2)
		if consumed {
			return errors.New("consumed a challenge of a removed factor")
		}
		return err
	})
	if err != nil {
		t.Fatalf("completing with a code during a disable: %v", err)
	}

	f.enable(t)
	live = f.challenge(t)
	backup := f.backup[0]
	err = f.disableWhileWaiting(t, func() error {
		_, consumed, err := f.repo.ConsumeMFABackupChallenge(ctx, live, backup)
		if consumed {
			return errors.New("consumed a challenge of a removed factor")
		}
		return err
	})
	if err != nil {
		t.Fatalf("completing with a backup code during a disable: %v", err)
	}

	// The real operations racing each other end without an error.
	for i := range 5 {
		f.enable(t)
		live := f.challenge(t)
		errs := make(chan error, 3)
		go func() {
			digest := sha256.Sum256([]byte(fmt.Sprint("next", i)))
			err := f.repo.CreateMFAChallenge(ctx, digest, f.membershipID, f.version, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour))
			if errors.Is(err, authpostgres.ErrMFAIdentityRequired) || errors.Is(err, auth.ErrCredentialsInvalid) {
				err = nil
			}
			errs <- err
		}()
		go func() {
			_, _, err := f.repo.ConsumeMFAChallenge(ctx, live, f.factor, 2)
			errs <- err
		}()
		go func() {
			_, err := f.repo.DisableMFA(ctx, f.userID, nil)
			errs <- err
		}()
		for range 3 {
			if err := <-errs; err != nil {
				t.Fatalf("race %d: %v", i, err)
			}
		}
	}
}

// TestMFAStepUpBackupCodeIsUsedUpByTheChangeAgainstPostgres: the store uses a
// step-up backup code up in the transaction of the change; a code that is not
// unused refuses the change, which then changes nothing.
func TestMFAStepUpBackupCodeIsUsedUpByTheChangeAgainstPostgres(t *testing.T) {
	f := newMFALockFixture(t)
	ctx := context.Background()
	unused := func(code auth.Digest) bool {
		t.Helper()
		found, err := f.repo.HasMFABackupCode(ctx, f.userID, code)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	for range 2 {
		if !unused(f.backup[0]) {
			t.Fatal("checking a backup code must not use it up")
		}
	}
	pending := sha256.Sum256([]byte("pending"))
	if err := f.repo.BeginTOTPRotation(ctx, f.userID, pending, []byte("pending"), time.Now().Add(10*time.Minute), &f.backup[0]); err != nil {
		t.Fatal(err)
	}
	if unused(f.backup[0]) {
		t.Fatal("starting the rotation must use its step-up code up")
	}
	for name, change := range map[string]func() error{
		"begin rotation": func() error {
			return f.repo.BeginTOTPRotation(ctx, f.userID, sha256.Sum256([]byte("other")), []byte("other"), time.Now().Add(10*time.Minute), &f.backup[0])
		},
		"confirm rotation": func() error {
			_, err := f.repo.ConfirmTOTPRotation(ctx, f.userID, pending, 5, f.backup, &f.backup[0])
			return err
		},
		"replace codes": func() error {
			_, err := f.repo.ReplaceMFABackupCodes(ctx, f.userID, f.backup, &f.backup[0])
			return err
		},
		"disable": func() error {
			_, err := f.repo.DisableMFA(ctx, f.userID, &f.backup[0])
			return err
		},
	} {
		if err := change(); !errors.Is(err, auth.ErrMFAStepUpInvalid) {
			t.Fatalf("%s with a used code: %v", name, err)
		}
	}
	record, found, err := f.repo.LoadPendingTOTP(ctx, f.userID)
	if err != nil || !found || record.SecretDigest != pending || !record.FactorActive {
		t.Fatalf("the pending rotation changed: found=%v record=%+v err=%v", found, record, err)
	}
	var factor []byte
	if err := f.db.QueryRow(ctx, "SELECT secret_digest FROM forge_totp_factors WHERE user_id = $1", f.userID).Scan(&factor); err != nil || string(factor) != string(f.factor[:]) {
		t.Fatalf("the factor changed: err=%v", err)
	}
	if !unused(f.backup[1]) {
		t.Fatal("a refused change used another code up")
	}
	confirmed, err := f.repo.ConfirmTOTPRotation(ctx, f.userID, pending, 5, f.backup, &f.backup[1])
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%v err=%v", confirmed, err)
	}
}
