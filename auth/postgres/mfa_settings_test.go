package postgres_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestMFASettingsAgainstPostgres(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	passwordHash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	ada, bob, gone := uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	for _, user := range []struct {
		id     uuid.UUID
		login  string
		status string
	}{{ada, "ada@example.com", "active"}, {bob, "bob@example.com", "active"}, {gone, "gone@example.com", "disabled"}} {
		if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash, status) VALUES ($1, $2, $2, $3, $4)",
			user.id, user.login, passwordHash, user.status); err != nil {
			t.Fatal(err)
		}
	}
	factorDigest := sha256.Sum256([]byte("ada-totp-secret"))
	for _, id := range []uuid.UUID{ada, gone} {
		if _, err := db.Exec(ctx, `
			INSERT INTO forge_totp_factors (user_id, secret_digest, secret_ciphertext, last_counter)
			VALUES ($1, $2, $3, 7)
		`, id, factorDigest[:], []byte("ciphertext")); err != nil {
			t.Fatal(err)
		}
	}
	backup := func(prefix string) []auth.Digest {
		digests := make([]auth.Digest, auth.DefaultMFABackupCodeCount)
		for i := range digests {
			digests[i] = sha256.Sum256(fmt.Appendf(nil, "%s-%d", prefix, i))
		}
		return digests
	}
	first := backup("first")
	for _, digest := range first {
		for _, id := range []uuid.UUID{ada, gone} {
			if _, err := db.Exec(ctx, "INSERT INTO forge_mfa_backup_codes (user_id, code_digest) VALUES ($1, $2)", id, digest[:]); err != nil {
				t.Fatal(err)
			}
		}
	}

	credential, found, err := repo.LookupSubjectPassword(ctx, ada)
	if err != nil || !found || credential.SubjectID != ada || credential.Login != "ada@example.com" || credential.PasswordHash != passwordHash {
		t.Fatalf("credential=%+v found=%v err=%v", credential.Login, found, err)
	}
	for _, id := range []uuid.UUID{gone, uuid.MustNew()} {
		if _, found, err := repo.LookupSubjectPassword(ctx, id); err != nil || found {
			t.Fatalf("inactive or unknown account found=%v err=%v", found, err)
		}
	}

	if status, err := repo.LoadMFAStatus(ctx, ada); err != nil || !status.Enabled || status.BackupCodesRemaining != auth.DefaultMFABackupCodeCount {
		t.Fatalf("ada status=%+v err=%v", status, err)
	}
	for _, id := range []uuid.UUID{bob, gone} {
		if status, err := repo.LoadMFAStatus(ctx, id); err != nil || status.Enabled || status.BackupCodesRemaining != 0 {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	}
	if record, found, err := repo.LoadActiveTOTP(ctx, ada); err != nil || !found || record.SecretDigest != factorDigest || record.LastCounter != 7 {
		t.Fatalf("active factor found=%v counter=%d err=%v", found, record.LastCounter, err)
	}
	if _, found, err := repo.LoadActiveTOTP(ctx, gone); err != nil || found {
		t.Fatalf("a disabled account's factor was loaded: found=%v err=%v", found, err)
	}

	// The replay watermark only moves forward, and one period is accepted
	// once even under concurrent requests.
	otherDigest := sha256.Sum256([]byte("other"))
	for _, attempt := range []struct {
		subject uuid.UUID
		digest  auth.Digest
		counter int64
	}{{ada, factorDigest, 7}, {ada, factorDigest, 6}, {ada, otherDigest, 9}, {bob, factorDigest, 9}, {gone, factorDigest, 9}} {
		if advanced, err := repo.AdvanceTOTPCounter(ctx, attempt.subject, attempt.digest, attempt.counter); err != nil || advanced {
			t.Fatalf("counter %d advanced=%v err=%v", attempt.counter, advanced, err)
		}
	}
	if accepted := concurrently(t, func() (bool, error) { return repo.AdvanceTOTPCounter(ctx, ada, factorDigest, 9) }); accepted != 1 {
		t.Fatalf("period accepted %d times", accepted)
	}

	// A backup code works once, for its own account with an active factor.
	if consumed, err := repo.ConsumeMFABackupCode(ctx, bob, first[0]); err != nil || consumed {
		t.Fatalf("another account used Ada's code: %v %v", consumed, err)
	}
	if consumed, err := repo.ConsumeMFABackupCode(ctx, gone, first[0]); err != nil || consumed {
		t.Fatalf("a disabled account used a code: %v %v", consumed, err)
	}
	if accepted := concurrently(t, func() (bool, error) { return repo.ConsumeMFABackupCode(ctx, ada, first[0]) }); accepted != 1 {
		t.Fatalf("backup code accepted %d times", accepted)
	}
	if status, _ := repo.LoadMFAStatus(ctx, ada); status.BackupCodesRemaining != auth.DefaultMFABackupCodeCount-1 {
		t.Fatalf("remaining=%d", status.BackupCodesRemaining)
	}

	second := backup("second")
	if _, err := repo.ReplaceMFABackupCodes(ctx, ada, second[:3], nil); err == nil {
		t.Fatal("a partial backup-code set was accepted")
	}
	if replaced, err := repo.ReplaceMFABackupCodes(ctx, bob, second, nil); err != nil || replaced {
		t.Fatalf("an account without a factor got backup codes: %v %v", replaced, err)
	}
	if replaced, err := repo.ReplaceMFABackupCodes(ctx, ada, second, nil); err != nil || !replaced {
		t.Fatalf("replaced=%v err=%v", replaced, err)
	}
	if consumed, err := repo.ConsumeMFABackupCode(ctx, ada, first[1]); err != nil || consumed {
		t.Fatalf("an old backup code still works: %v %v", consumed, err)
	}
	if consumed, err := repo.ConsumeMFABackupCode(ctx, ada, second[1]); err != nil || !consumed {
		t.Fatalf("a new backup code does not work: %v %v", consumed, err)
	}
	if status, _ := repo.LoadMFAStatus(ctx, ada); status.BackupCodesRemaining != auth.DefaultMFABackupCodeCount-1 {
		t.Fatalf("remaining after regeneration=%d", status.BackupCodesRemaining)
	}
}

// concurrently runs attempt from eight goroutines and counts the successes.
func concurrently(t *testing.T, attempt func() (bool, error)) int {
	t.Helper()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for range 8 {
		wg.Go(func() {
			ok, err := attempt()
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return accepted
}
