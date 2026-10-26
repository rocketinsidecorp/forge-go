package postgres_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestPasswordRecoveryConsumesOnceAndRevokesSessions(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "alice@example.com", "alice@example.com", "Acme", "acme")

	identity, found, err := repo.LookupRecovery(ctx, "alice@example.com", "acme")
	if err != nil || !found || identity.SubjectID != userID {
		t.Fatalf("identity=%+v found=%v err=%v", identity, found, err)
	}

	sessionToken, err := repo.CreateSession(ctx, membershipID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	first, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRecovery(ctx, userID, first.Digest(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRecovery(ctx, userID, second.Digest(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	newHash, err := auth.HashPassword("replacement password")
	if err != nil {
		t.Fatal(err)
	}
	if consumed, err := repo.ConsumeRecovery(ctx, first.Digest(), newHash); err != nil || consumed {
		t.Fatalf("superseded token consumed=%v err=%v", consumed, err)
	}
	consumed, err := repo.ConsumeRecovery(ctx, second.Digest(), newHash)
	if err != nil || !consumed {
		t.Fatalf("active token consumed=%v err=%v", consumed, err)
	}
	if consumed, err := repo.ConsumeRecovery(ctx, second.Digest(), newHash); err != nil || consumed {
		t.Fatalf("reused token consumed=%v err=%v", consumed, err)
	}
	if _, found, err := repo.ResolveSession(ctx, sessionToken.Digest()); err != nil || found {
		t.Fatalf("old session found=%v err=%v", found, err)
	}

	var storedHash string
	var sessionVersion int64
	if err := db.QueryRow(ctx, "SELECT password_hash, session_version FROM forge_users WHERE id = $1", userID).Scan(&storedHash, &sessionVersion); err != nil {
		t.Fatal(err)
	}
	match, _, err := auth.VerifyPassword(storedHash, "replacement password")
	if err != nil || !match || sessionVersion != 2 {
		t.Fatalf("password match=%v version=%d err=%v", match, sessionVersion, err)
	}
}

func TestPasswordRecoveryConcurrentTokensOnlyOneWins(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "race@example.com", "race@example.com", "Race", "race")

	tokenA, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	for _, digest := range []auth.Digest{tokenA.Digest(), tokenB.Digest()} {
		if _, err := db.Exec(ctx, `
			INSERT INTO forge_password_recovery (id, token_digest, user_id, expires_at)
			VALUES ($4, $1, $2, $3)
		`, digest[:], userID, expires, uuid.MustNew()); err != nil {
			t.Fatal(err)
		}
	}

	hashA, err := auth.HashPassword("password A")
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := auth.HashPassword("password B")
	if err != nil {
		t.Fatal(err)
	}

	var winners atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, attempt := range []struct {
		digest auth.Digest
		hash   string
	}{
		{tokenA.Digest(), hashA},
		{tokenB.Digest(), hashB},
	} {
		attempt := attempt
		wg.Add(1)
		go func() {
			defer wg.Done()
			consumed, err := repo.ConsumeRecovery(context.Background(), attempt.digest, attempt.hash)
			if consumed {
				winners.Add(1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if winners.Load() != 1 {
		t.Fatalf("winners=%d want 1", winners.Load())
	}

	var sessionVersion int64
	var storedHash string
	if err := db.QueryRow(ctx, "SELECT password_hash, session_version FROM forge_users WHERE id = $1", userID).Scan(&storedHash, &sessionVersion); err != nil {
		t.Fatal(err)
	}
	matchA, _, err := auth.VerifyPassword(storedHash, "password A")
	if err != nil {
		t.Fatal(err)
	}
	matchB, _, err := auth.VerifyPassword(storedHash, "password B")
	if err != nil {
		t.Fatal(err)
	}
	if sessionVersion != 2 || matchA == matchB {
		t.Fatalf("session_version=%d matchA=%v matchB=%v", sessionVersion, matchA, matchB)
	}
}

func TestPasswordRecoveryPrunesExpiredAndConsumedRows(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "prune@example.com", "prune@example.com", "Prune", "prune")

	for i, consumed := range []bool{false, true, false} {
		token, err := auth.NewRecoveryToken()
		if err != nil {
			t.Fatal(err)
		}
		expires := "now() + interval '1 hour'"
		if i == 0 {
			expires = "now() - interval '1 hour'"
		}
		consumedSQL := "NULL"
		if consumed {
			consumedSQL = "now()"
		}
		query := "INSERT INTO forge_password_recovery (id, token_digest, user_id, expires_at, consumed_at) VALUES ($3, $1, $2, " + expires + ", " + consumedSQL + ")"
		digest := token.Digest()
		if _, err := db.Exec(ctx, query, digest[:], userID, uuid.MustNew()); err != nil {
			t.Fatal(err)
		}
	}

	pruned, err := repo.PruneRecovery(ctx, 10)
	if err != nil || pruned != 2 {
		t.Fatalf("pruned=%d err=%v", pruned, err)
	}
	var remaining int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM forge_password_recovery").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining=%d", remaining)
	}
}

func TestPasswordRecoveryRejectsExpiredToken(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "expired@example.com", "expired@example.com", "Expired", "expired")

	token, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	digest := token.Digest()
	if _, err := db.Exec(ctx, `
		INSERT INTO forge_password_recovery (id, token_digest, user_id, expires_at)
		VALUES ($3, $1, $2, now() - interval '1 second')
	`, digest[:], userID, uuid.MustNew()); err != nil {
		t.Fatal(err)
	}
	nextHash, err := auth.HashPassword("replacement password")
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := repo.ConsumeRecovery(ctx, digest, nextHash)
	if err != nil || consumed {
		t.Fatalf("consumed=%v err=%v", consumed, err)
	}
}

func TestRecoveryTokenUsableRequiresAnUnusedUnexpiredToken(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "usable@example.com", "usable@example.com", "Usable", "usable")

	usable := func(token auth.RecoveryToken) bool {
		t.Helper()
		ok, err := repo.RecoveryTokenUsable(ctx, token.Digest())
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	unknown, _ := auth.NewRecoveryToken()
	if usable(unknown) {
		t.Fatal("an unknown token is usable")
	}

	live, _ := auth.NewRecoveryToken()
	if err := repo.CreateRecovery(ctx, userID, live.Digest(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !usable(live) {
		t.Fatal("a fresh token is not usable")
	}

	expired, _ := auth.NewRecoveryToken()
	expiredDigest := expired.Digest()
	if _, err := db.Exec(ctx, "INSERT INTO forge_password_recovery (id, token_digest, user_id, expires_at) VALUES ($3, $1, $2, now() - interval '1 second')", expiredDigest[:], userID, uuid.MustNew()); err != nil {
		t.Fatal(err)
	}
	if usable(expired) {
		t.Fatal("an expired token is usable")
	}

	// A newer request supersedes the older token.
	newer, _ := auth.NewRecoveryToken()
	if err := repo.CreateRecovery(ctx, userID, newer.Digest(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if usable(live) || !usable(newer) {
		t.Fatal("a superseded token is still usable, or the newest is not")
	}

	hash, err := auth.HashPassword("replacement password")
	if err != nil {
		t.Fatal(err)
	}
	if consumed, err := repo.ConsumeRecovery(ctx, newer.Digest(), hash); err != nil || !consumed {
		t.Fatalf("consumed=%v err=%v", consumed, err)
	}
	if usable(newer) {
		t.Fatal("a used token is usable")
	}

	// Through the service: unknown, used and expired tokens are refused
	// without the password being hashed (a password HashPassword rejects would
	// otherwise answer ErrPasswordInvalid).
	service, err := auth.NewRecoveryService(repo, nopSender{})
	if err != nil {
		t.Fatal(err)
	}
	tooLong := string(make([]byte, 5000))
	for name, token := range map[string]auth.RecoveryToken{"unknown": unknown, "used": newer, "expired": expired} {
		err := service.Reset(ctx, auth.PasswordReset{Token: token.Reveal(), NewPassword: tooLong})
		if !errors.Is(err, auth.ErrRecoveryInvalid) {
			t.Errorf("%s token: %v, want ErrRecoveryInvalid before any password work", name, err)
		}
	}
}

type nopSender struct{}

func (nopSender) SendPasswordRecovery(context.Context, auth.PasswordRecoveryMessage) error {
	return nil
}
