package postgres_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestTOTPLoginFlowAgainstPostgres(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	passwordHash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, $2, $3, $4)", userID, "Alice@example.com", "alice@example.com", passwordHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_organizations (id, name) VALUES ($1, 'Acme')", orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, 'acme')", tenantID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", membershipID, userID, tenantID); err != nil {
		t.Fatal(err)
	}

	oldSession, err := repo.CreateSession(ctx, membershipID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	mfa, err := auth.NewMFAService(repo, repo, secretCipher, auth.WithMFAClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := mfa.BeginTOTPEnrollment(ctx, userID, "Forge", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	rawSecret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	code := testTOTPCode(rawSecret, now)
	confirmation, err := mfa.ConfirmTOTPEnrollment(ctx, userID, code)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmation.BackupCodes) != auth.DefaultMFABackupCodeCount {
		t.Fatalf("backup code count=%d", len(confirmation.BackupCodes))
	}
	if _, err := mfa.BeginTOTPEnrollment(ctx, userID, "Forge", "alice@example.com"); !errors.Is(err, auth.ErrMFAAlreadyEnabled) {
		t.Fatalf("second enrollment error=%v", err)
	}
	if _, found, err := repo.ResolveSession(ctx, oldSession.Digest()); err != nil || found {
		t.Fatalf("pre-MFA session found=%v err=%v", found, err)
	}

	now = now.Add(30 * time.Second)
	login, err := auth.NewLoginService(
		repo,
		repo,
		auth.WithLoginClock(clock),
		auth.WithMFAChallengeIssuer(mfa),
	)
	if err != nil {
		t.Fatal(err)
	}
	firstFactor, err := login.Login(ctx, auth.LoginInput{
		Email:      "alice@example.com",
		Password:   "correct-password",
		TenantSlug: "acme",
		Source:     "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !firstFactor.MFARequired || firstFactor.MFAChallenge.Reveal() == "" {
		t.Fatalf("first factor result=%+v", firstFactor)
	}

	code = testTOTPCode(rawSecret, now)
	completed, err := mfa.CompleteLogin(ctx, auth.MFACompletion{
		ChallengeToken: firstFactor.MFAChallenge.Reveal(),
		Code:           code,
		Source:         "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.ResolveSession(ctx, completed.Token.Digest()); err != nil || !found {
		t.Fatalf("MFA session found=%v err=%v", found, err)
	}
	if _, err := mfa.CompleteLogin(ctx, auth.MFACompletion{
		ChallengeToken: firstFactor.MFAChallenge.Reveal(),
		Code:           code,
		Source:         "192.0.2.10",
	}); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("replayed challenge error=%v", err)
	}

	now = now.Add(30 * time.Second)
	backupChallenge, err := login.Login(ctx, auth.LoginInput{
		Email:      "alice@example.com",
		Password:   "correct-password",
		TenantSlug: "acme",
		Source:     "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	backupCode := confirmation.BackupCodes[0].Reveal()
	backupSession, err := mfa.CompleteLogin(ctx, auth.MFACompletion{
		ChallengeToken: backupChallenge.MFAChallenge.Reveal(),
		Code:           backupCode,
		Source:         "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.ResolveSession(ctx, backupSession.Token.Digest()); err != nil || !found {
		t.Fatalf("backup-code session found=%v err=%v", found, err)
	}

	replayChallenge, err := login.Login(ctx, auth.LoginInput{
		Email:      "alice@example.com",
		Password:   "correct-password",
		TenantSlug: "acme",
		Source:     "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mfa.CompleteLogin(ctx, auth.MFACompletion{
		ChallengeToken: replayChallenge.MFAChallenge.Reveal(),
		Code:           backupCode,
		Source:         "192.0.2.10",
	}); !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("replayed backup code error=%v", err)
	}

	var remaining int
	if err := db.QueryRow(ctx, `
		SELECT count(*)
		FROM forge_mfa_backup_codes
		WHERE user_id = $1 AND consumed_at IS NULL
	`, userID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != auth.DefaultMFABackupCodeCount-1 {
		t.Fatalf("remaining backup codes=%d", remaining)
	}
}

func TestMFAChallengeDiesAfterCredentialVersionChange(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	passwordHash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, 'bob@example.com', 'bob@example.com', $2)", userID, passwordHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_organizations (id, name) VALUES ($1, 'Example')", orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, 'example')", tenantID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", membershipID, userID, tenantID); err != nil {
		t.Fatal(err)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	mfa, err := auth.NewMFAService(repo, repo, secretCipher, auth.WithMFAClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := mfa.BeginTOTPEnrollment(ctx, userID, "Forge", "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	rawSecret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mfa.ConfirmTOTPEnrollment(ctx, userID, testTOTPCode(rawSecret, now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	challenge, _, err := mfa.IssueMFAChallenge(ctx, membershipID, 2, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RevokeUserSessions(ctx, userID); err != nil {
		t.Fatal(err)
	}
	_, err = mfa.CompleteLogin(ctx, auth.MFACompletion{
		ChallengeToken: challenge.Reveal(),
		Code:           testTOTPCode(rawSecret, now),
	})
	if !errors.Is(err, auth.ErrMFAInvalid) {
		t.Fatalf("credential-version invalidation error=%v", err)
	}
}

func testTOTPCode(secret []byte, now time.Time) string {
	// RFC 6238 uses HOTP(counter = UnixTime / period). Keep this tiny helper
	// independent of auth internals to exercise the public MFA path.
	counter := uint64(now.Unix() / 30)
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (((uint32(sum[offset]) & 0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])) % 1_000_000
	return fmt.Sprintf("%06d", value)
}

func TestMFARotationAndDisableAgainstPostgres(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	passwordHash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, 'rotate@example.com', 'rotate@example.com', $2)", userID, passwordHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_organizations (id, name) VALUES ($1, 'Rotate')", orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, 'rotate')", tenantID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", membershipID, userID, tenantID); err != nil {
		t.Fatal(err)
	}

	oldDigest := sha256.Sum256([]byte("old-totp-secret"))
	if _, err := db.Exec(ctx, `
		INSERT INTO forge_totp_factors
			(user_id, secret_digest, secret_ciphertext, last_counter)
		VALUES ($1, $2, $3, 7)
	`, userID, oldDigest[:], []byte("old-ciphertext")); err != nil {
		t.Fatal(err)
	}

	oldSession, err := repo.CreateSession(ctx, membershipID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	newDigest := sha256.Sum256([]byte("new-totp-secret"))
	if err := repo.BeginTOTPRotation(ctx, userID, newDigest, []byte("new-ciphertext"), time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	backupDigests := make([]auth.Digest, auth.DefaultMFABackupCodeCount)
	for i := range backupDigests {
		backupDigests[i] = sha256.Sum256([]byte(fmt.Sprintf("backup-%d", i)))
	}
	confirmed, err := repo.ConfirmTOTPRotation(ctx, userID, newDigest, 123, backupDigests)
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%v err=%v", confirmed, err)
	}

	var (
		storedDigest []byte
		lastCounter  int64
		version      int64
		backupCount  int
	)
	if err := db.QueryRow(ctx, "SELECT secret_digest, last_counter FROM forge_totp_factors WHERE user_id = $1", userID).Scan(&storedDigest, &lastCounter); err != nil {
		t.Fatal(err)
	}
	if string(storedDigest) != string(newDigest[:]) || lastCounter != 123 {
		t.Fatalf("factor digest=%x counter=%d", storedDigest, lastCounter)
	}
	if err := db.QueryRow(ctx, "SELECT count(*) FROM forge_mfa_backup_codes WHERE user_id = $1 AND consumed_at IS NULL", userID).Scan(&backupCount); err != nil {
		t.Fatal(err)
	}
	if backupCount != auth.DefaultMFABackupCodeCount {
		t.Fatalf("backup count=%d", backupCount)
	}
	if err := db.QueryRow(ctx, "SELECT session_version FROM forge_users WHERE id = $1", userID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("session version after rotation=%d", version)
	}
	if _, found, err := repo.ResolveSession(ctx, oldSession.Digest()); err != nil || found {
		t.Fatalf("pre-rotation session found=%v err=%v", found, err)
	}

	newSession, err := repo.CreateSession(ctx, membershipID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := repo.DisableMFA(ctx, userID)
	if err != nil || !disabled {
		t.Fatalf("disabled=%v err=%v", disabled, err)
	}
	var factorCount int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM forge_totp_factors WHERE user_id = $1", userID).Scan(&factorCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "SELECT count(*) FROM forge_mfa_backup_codes WHERE user_id = $1", userID).Scan(&backupCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "SELECT session_version FROM forge_users WHERE id = $1", userID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if factorCount != 0 || backupCount != 0 || version != 3 {
		t.Fatalf("factor=%d backup=%d version=%d", factorCount, backupCount, version)
	}
	if _, found, err := repo.ResolveSession(ctx, newSession.Digest()); err != nil || found {
		t.Fatalf("pre-disable session found=%v err=%v", found, err)
	}
}
