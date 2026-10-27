package auth

import (
	"context"
	"encoding/base32"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// mfaDomainFixture is MFAService over memoryMFAAccounts with two accounts,
// ada and bob, on a fixed clock: the two-factor changes driven directly,
// with the proofs this package mints.
type mfaDomainFixture struct {
	t        *testing.T
	accounts *memoryMFAAccounts
	mfa      *MFAService
	ada, bob uuid.UUID
	clockMu  sync.Mutex
	now      time.Time
}

func newMFADomainFixture(t *testing.T) *mfaDomainFixture {
	t.Helper()
	hash, err := cheapPasswordHash(mfaTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	f := &mfaDomainFixture{t: t, ada: uuid.MustNew(), bob: uuid.MustNew(), now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	f.accounts = &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{
		f.ada: {login: "ada@example.com", hash: hash},
		f.bob: {login: "bob@example.com", hash: hash},
	}}
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if f.mfa, err = NewMFAService(f.accounts, &sessionStub{}, cipher, WithMFAClock(f.clock)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *mfaDomainFixture) clock() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.now
}

func (f *mfaDomainFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = f.now.Add(d)
}

// code is the authenticator code of base32Key now.
func (f *mfaDomainFixture) code(base32Key string) string {
	f.t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(base32Key)
	if err != nil {
		f.t.Fatal(err)
	}
	return totpCode(secret, uint64(f.clock().Unix()/totpPeriodSeconds))
}

func (f *mfaDomainFixture) status(subject uuid.UUID) MFAStatus {
	f.t.Helper()
	status, err := f.accounts.LoadMFAStatus(context.Background(), subject)
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

// enable turns MFA on for subject and returns the key and backup codes.
func (f *mfaDomainFixture) enable(subject uuid.UUID) (string, []string) {
	f.t.Helper()
	ctx := context.Background()
	enrollment, err := f.mfa.BeginTOTPEnrollment(ctx, subject, "Forge", f.accounts.account(subject).login)
	if err != nil {
		f.t.Fatal(err)
	}
	key := enrollment.Secret.Reveal()
	confirmation, err := f.mfa.ConfirmTOTPEnrollment(ctx, subject, f.code(key))
	if err != nil {
		f.t.Fatal(err)
	}
	f.advance(totpPeriodSeconds * time.Second)
	codes := make([]string, 0, len(confirmation.BackupCodes))
	for _, code := range confirmation.BackupCodes {
		codes = append(codes, code.Reveal())
	}
	return key, codes
}

// TestMFAStepUpBackupCodeUsedMeanwhileRefusesTheChange: a step-up backup
// code used elsewhere after it was checked refuses every change with
// ErrMFAStepUpInvalid, and nothing changes; a TOTP step-up is used up at once.
func TestMFAStepUpBackupCodeUsedMeanwhileRefusesTheChange(t *testing.T) {
	t.Parallel()
	f := newMFADomainFixture(t)
	ctx := context.Background()
	key, backup := f.enable(f.ada)
	rotation, err := f.mfa.BeginTOTPRotation(ctx, f.ada, "Forge", "ada@example.com", changeAuthorizedBy(f.ada))
	if err != nil {
		t.Fatal(err)
	}
	proof, err := f.mfa.VerifyMFAStepUp(ctx, f.ada, backup[0])
	if err != nil || !proof.BackupCode() {
		t.Fatalf("proof=%+v err=%v", proof, err)
	}
	if _, err := f.mfa.VerifyMFAStepUp(ctx, f.ada, backup[0]); err != nil {
		t.Fatalf("checking a backup code must not use it up: %v", err)
	}
	if consumed, err := f.accounts.ConsumeMFABackupCode(ctx, f.ada, mustBackupDigest(t, backup[0])); err != nil || !consumed {
		t.Fatalf("consumed=%v err=%v", consumed, err)
	}
	if _, err := f.mfa.ConfirmTOTPRotation(ctx, f.ada, f.code(rotation.Secret.Reveal()), proof); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Fatalf("confirm rotation err=%v", err)
	}
	if _, err := f.mfa.RegenerateMFABackupCodes(ctx, f.ada, proof); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Fatalf("regenerate err=%v", err)
	}
	if err := f.mfa.DisableMFA(ctx, f.ada, proof); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Fatalf("disable err=%v", err)
	}
	account := f.accounts.account(f.ada)
	if account.factor == nil || account.pending == nil || f.status(f.ada).BackupCodesRemaining != DefaultMFABackupCodeCount-1 {
		t.Fatal("a refused change must change nothing")
	}
	totp := f.code(key)
	if proof, err := f.mfa.VerifyMFAStepUp(ctx, f.ada, totp); err != nil || proof.BackupCode() {
		t.Fatalf("totp proof=%+v err=%v", proof, err)
	}
	if _, err := f.mfa.VerifyMFAStepUp(ctx, f.ada, totp); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("a TOTP step-up must be used up at once: %v", err)
	}
	if _, err := f.mfa.VerifyMFAStepUp(ctx, f.bob, backup[1]); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("another account's backup code: %v", err)
	}
}

// TestPendingTOTPIsNeverReadAsTheOtherKind: a pending key is a rotation's
// while the account has a factor and an enrollment's otherwise, so a
// confirmation of the other kind never checks it and PendingTOTP never shows
// it.
func TestPendingTOTPIsNeverReadAsTheOtherKind(t *testing.T) {
	t.Parallel()
	f := newMFADomainFixture(t)
	ctx := context.Background()
	f.enable(f.ada)
	rotation, err := f.mfa.BeginTOTPRotation(ctx, f.ada, "Forge", "ada@example.com", changeAuthorizedBy(f.ada))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"000000", f.code(rotation.Secret.Reveal())} {
		if _, err := f.mfa.ConfirmTOTPEnrollment(ctx, f.ada, code); !errors.Is(err, ErrMFAAlreadyEnabled) {
			t.Fatalf("enrollment confirmation of a pending rotation: %v", err)
		}
	}
	if _, found, err := f.mfa.PendingTOTP(ctx, f.ada, "Forge", "ada@example.com", false); err != nil || found {
		t.Fatalf("a rotation's key shown as an enrollment's: found=%v err=%v", found, err)
	}
	if _, found, err := f.mfa.PendingTOTP(ctx, f.ada, "Forge", "ada@example.com", true); err != nil || !found {
		t.Fatalf("rotation key: found=%v err=%v", found, err)
	}

	enrollment, err := f.mfa.BeginTOTPEnrollment(ctx, f.bob, "Forge", "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"000000", f.code(enrollment.Secret.Reveal())} {
		if _, err := f.mfa.ConfirmTOTPRotation(ctx, f.bob, code, changeAuthorizedBy(f.bob)); !errors.Is(err, ErrMFANotEnabled) {
			t.Fatalf("rotation confirmation of a pending enrollment: %v", err)
		}
	}
	if _, found, err := f.mfa.PendingTOTP(ctx, f.bob, "Forge", "bob@example.com", true); err != nil || found {
		t.Fatalf("an enrollment's key shown as a rotation's: found=%v err=%v", found, err)
	}
}

// TestMFAStepUpProofIsBoundToItsAccount: every change of an existing factor
// requires a proof, refuses one of another account and the zero value, and
// then changes and uses up nothing.
func TestMFAStepUpProofIsBoundToItsAccount(t *testing.T) {
	t.Parallel()
	f := newMFADomainFixture(t)
	ctx := context.Background()
	adaKey, adaBackup := f.enable(f.ada)
	bobKey, _ := f.enable(f.bob)
	backupProof, err := f.mfa.VerifyMFAStepUp(ctx, f.ada, adaBackup[0])
	if err != nil || backupProof.Subject() != f.ada {
		t.Fatalf("proof=%+v err=%v", backupProof, err)
	}
	totpProof, err := f.mfa.VerifyMFAStepUp(ctx, f.ada, f.code(adaKey))
	if err != nil {
		t.Fatal(err)
	}
	for name, proof := range map[string]MFAStepUpProof{"backup code": backupProof, "TOTP": totpProof, "zero": {}} {
		if _, err := f.mfa.BeginTOTPRotation(ctx, f.bob, "Forge", "bob@example.com", proof); !errors.Is(err, ErrMFAStepUpInvalid) {
			t.Fatalf("%s: begin rotation of another account: %v", name, err)
		}
		if _, err := f.mfa.ConfirmTOTPRotation(ctx, f.bob, f.code(bobKey), proof); !errors.Is(err, ErrMFAStepUpInvalid) {
			t.Fatalf("%s: confirm rotation of another account: %v", name, err)
		}
		if _, err := f.mfa.RegenerateMFABackupCodes(ctx, f.bob, proof); !errors.Is(err, ErrMFAStepUpInvalid) {
			t.Fatalf("%s: regenerate another account's codes: %v", name, err)
		}
		if err := f.mfa.DisableMFA(ctx, f.bob, proof); !errors.Is(err, ErrMFAStepUpInvalid) {
			t.Fatalf("%s: disable another account: %v", name, err)
		}
	}
	if err := f.mfa.DisableMFA(ctx, f.ada, MFAStepUpProof{}); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Fatalf("the zero proof: %v", err)
	}
	for _, subject := range []uuid.UUID{f.ada, f.bob} {
		status := f.status(subject)
		if !status.Enabled || status.BackupCodesRemaining != DefaultMFABackupCodeCount || f.accounts.account(subject).pending != nil {
			t.Fatalf("a refused change changed %v: %+v", subject, status)
		}
	}
	if err := f.mfa.DisableMFA(ctx, f.ada, backupProof); err != nil {
		t.Fatalf("ada's own proof: %v", err)
	}
}
