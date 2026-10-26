package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// These tests check the behavior of the security invariants the store owns
// against PostgreSQL: the password rehash compare-and-set, recovery
// invalidation and MFA pruning. failures_test.go covers their error paths.

type identityIDs struct{ user, org, tenant, membership uuid.UUID }

func newIdentity(t *testing.T, db *postgres.DB, name string) identityIDs {
	t.Helper()
	ids := identityIDs{uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()}
	insertIdentityFixture(t, db, ids.user, ids.org, ids.tenant, ids.membership, name+"@example.com", name+"@example.com", name, name)
	return ids
}

func storedPasswordHash(t *testing.T, db *postgres.DB, user uuid.UUID) string {
	t.Helper()
	var hash string
	if err := db.QueryRow(context.Background(), "SELECT password_hash FROM forge_users WHERE id = $1", user).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestReplacePasswordHashIsACompareAndSet(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := newIdentity(t, db, "alice")
	other := newIdentity(t, db, "bob")

	replaced, err := repo.ReplacePasswordHash(ctx, alice.user, "test-only", "rehashed-1")
	if err != nil || !replaced {
		t.Fatalf("matching hash: replaced=%v err=%v", replaced, err)
	}
	if got := storedPasswordHash(t, db, alice.user); got != "rehashed-1" {
		t.Fatalf("stored hash = %q", got)
	}
	// The hash the caller verified is no longer current: a replay, or a
	// rehash that lost a race, changes nothing.
	if replaced, err := repo.ReplacePasswordHash(ctx, alice.user, "test-only", "rehashed-2"); err != nil || replaced {
		t.Fatalf("stale hash: replaced=%v err=%v", replaced, err)
	}
	if got := storedPasswordHash(t, db, alice.user); got != "rehashed-1" {
		t.Fatalf("a stale rehash overwrote the hash: %q", got)
	}
	// Another account's hash is never touched.
	if got := storedPasswordHash(t, db, other.user); got != "test-only" {
		t.Fatalf("another account's hash = %q", got)
	}
	// A disabled account keeps the hash it has.
	if _, err := db.Exec(ctx, "UPDATE forge_users SET status = 'disabled' WHERE id = $1", other.user); err != nil {
		t.Fatal(err)
	}
	if replaced, err := repo.ReplacePasswordHash(ctx, other.user, "test-only", "rehashed-3"); err != nil || replaced {
		t.Fatalf("disabled account: replaced=%v err=%v", replaced, err)
	}
	// An unknown account is not an error, only a miss.
	if replaced, err := repo.ReplacePasswordHash(ctx, uuid.MustNew(), "test-only", "rehashed-4"); err != nil || replaced {
		t.Fatalf("unknown account: replaced=%v err=%v", replaced, err)
	}
}

func TestConcurrentRehashesLetExactlyOneWin(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := newIdentity(t, db, "alice")
	const attempts = 12
	var wins atomic.Int32
	winner := make(chan string, attempts)
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			next := fmt.Sprintf("rehashed-%d", i)
			<-start
			replaced, err := repo.ReplacePasswordHash(ctx, alice.user, "test-only", next)
			if err != nil {
				t.Errorf("attempt %d: %v", i, err)
				return
			}
			if replaced {
				wins.Add(1)
				winner <- next
			}
		}()
	}
	close(start)
	group.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d rehashes succeeded from one verified hash, want exactly 1", wins.Load())
	}
	if got, want := storedPasswordHash(t, db, alice.user), <-winner; got != want {
		t.Fatalf("stored hash %q is not the winner's %q", got, want)
	}
}

// A rehash that verified the old password must not undo a password reset that
// completed in between.
func TestRehashCannotOverwriteAPasswordReset(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := newIdentity(t, db, "alice")
	token, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRecovery(ctx, alice.user, token.Digest(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	resetHash, err := auth.HashPassword("the reset password")
	if err != nil {
		t.Fatal(err)
	}
	if consumed, err := repo.ConsumeRecovery(ctx, token.Digest(), resetHash); err != nil || !consumed {
		t.Fatalf("reset: consumed=%v err=%v", consumed, err)
	}
	// The login that started before the reset still holds the old hash.
	if replaced, err := repo.ReplacePasswordHash(ctx, alice.user, "test-only", "stale-rehash"); err != nil || replaced {
		t.Fatalf("rehash after reset: replaced=%v err=%v", replaced, err)
	}
	if got := storedPasswordHash(t, db, alice.user); got != resetHash {
		t.Fatalf("the reset password was overwritten: %q", got)
	}
}

func TestInvalidateRecoveryBurnsTheLinkOnly(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := newIdentity(t, db, "alice")
	bob := newIdentity(t, db, "bob")
	newToken := func(user uuid.UUID) auth.RecoveryToken {
		t.Helper()
		token, err := auth.NewRecoveryToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.CreateRecovery(ctx, user, token.Digest(), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		return token
	}
	aliceToken, bobToken := newToken(alice.user), newToken(bob.user)
	newHash, err := auth.HashPassword("a new password")
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.InvalidateRecovery(ctx, aliceToken.Digest()); err != nil {
		t.Fatal(err)
	}
	if consumed, err := repo.ConsumeRecovery(ctx, aliceToken.Digest(), newHash); err != nil || consumed {
		t.Fatalf("an invalidated link was consumed=%v err=%v", consumed, err)
	}
	if got := storedPasswordHash(t, db, alice.user); got != "test-only" {
		t.Fatalf("an invalidated link changed the password: %q", got)
	}
	// Repeating it, and naming a link that never existed, are harmless.
	if err := repo.InvalidateRecovery(ctx, aliceToken.Digest()); err != nil {
		t.Fatalf("second invalidation: %v", err)
	}
	unknown, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.InvalidateRecovery(ctx, unknown.Digest()); err != nil {
		t.Fatalf("unknown link: %v", err)
	}
	// Another account's link still works.
	if consumed, err := repo.ConsumeRecovery(ctx, bobToken.Digest(), newHash); err != nil || !consumed {
		t.Fatalf("another account's link: consumed=%v err=%v", consumed, err)
	}
	// Invalidating a link that was already used keeps the time it was used.
	var before, after time.Time
	read := func(dst *time.Time) {
		digest := bobToken.Digest()
		if err := db.QueryRow(ctx, "SELECT consumed_at FROM forge_password_recovery WHERE token_digest = $1", digest[:]).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	read(&before)
	if err := repo.InvalidateRecovery(ctx, bobToken.Digest()); err != nil {
		t.Fatal(err)
	}
	read(&after)
	if !before.Equal(after) {
		t.Fatalf("consumed_at moved from %v to %v", before, after)
	}
}

func TestPruneMFADeletesOnlyDeadRowsAndHonorsTheLimit(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repo.PruneMFA(ctx, 0); err == nil {
		t.Fatal("a zero limit was accepted")
	}
	digest := func(seed byte) []byte {
		d := make([]byte, 32)
		for i := range d {
			d[i] = seed
		}
		return d
	}
	// Enrollments: one expired, one live (a user has at most one).
	expired, live := newIdentity(t, db, "expired"), newIdentity(t, db, "live")
	for user, window := range map[uuid.UUID]string{expired.user: "- interval '1 minute'", live.user: "+ interval '1 hour'"} {
		if _, err := db.Exec(ctx, "INSERT INTO forge_totp_enrollments (user_id, secret_digest, secret_ciphertext, expires_at) VALUES ($1, $2, '\\x01', now() "+window+")", user, digest(1)); err != nil {
			t.Fatal(err)
		}
	}
	// Challenges: live, consumed, expired and session-expired.
	challenges := []struct {
		seed     byte
		consumed string
		expires  string
		session  string
		dead     bool
	}{
		{1, "NULL", "+ interval '1 hour'", "+ interval '1 hour'", false},
		{2, "now()", "+ interval '1 hour'", "+ interval '1 hour'", true},
		{3, "NULL", "- interval '1 minute'", "+ interval '1 hour'", true},
		{4, "NULL", "+ interval '1 hour'", "- interval '1 minute'", true},
	}
	for _, c := range challenges {
		query := "INSERT INTO forge_mfa_challenges (id, token_digest, membership_id, credential_version, factor_secret_digest, expires_at, session_expires_at, consumed_at) VALUES ($4, $1, $2, 1, $3, now() " + c.expires + ", now() " + c.session + ", " + c.consumed + ")"
		if _, err := db.Exec(ctx, query, digest(c.seed), live.membership, digest(9), uuid.MustNew()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(table string) int {
		var n int
		if err := db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The limit caps each category: one dead enrollment and one dead challenge.
	if pruned, err := repo.PruneMFA(ctx, 1); err != nil || pruned != 2 {
		t.Fatalf("limited prune: pruned=%d err=%v", pruned, err)
	}
	if enrollments, challengeRows := count("forge_totp_enrollments"), count("forge_mfa_challenges"); enrollments != 1 || challengeRows != 3 {
		t.Fatalf("after a limited prune: enrollments=%d challenges=%d", enrollments, challengeRows)
	}
	if pruned, err := repo.PruneMFA(ctx, 10); err != nil || pruned != 2 {
		t.Fatalf("second prune: pruned=%d err=%v", pruned, err)
	}
	if enrollments, challengeRows := count("forge_totp_enrollments"), count("forge_mfa_challenges"); enrollments != 1 || challengeRows != 1 {
		t.Fatalf("after pruning: enrollments=%d challenges=%d, want only the live ones", enrollments, challengeRows)
	}
	var survivor []byte
	if err := db.QueryRow(ctx, "SELECT token_digest FROM forge_mfa_challenges").Scan(&survivor); err != nil || string(survivor) != string(digest(1)) {
		t.Fatalf("the surviving challenge is not the live one: %v %x", err, survivor)
	}
	if pruned, err := repo.PruneMFA(ctx, 10); err != nil || pruned != 0 {
		t.Fatalf("nothing is left to prune: pruned=%d err=%v", pruned, err)
	}
}
