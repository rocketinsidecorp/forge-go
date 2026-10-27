package authhttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestLoginHandlerReturnsMFAChallengeWithoutSession(t *testing.T) {
	hash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := auth.NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	membershipID := uuid.MustNew()
	store := &loginStoreStub{
		found: true,
		identity: auth.PasswordIdentity{
			SubjectID:         uuid.MustNew(),
			MembershipID:      membershipID,
			PasswordHash:      hash,
			CredentialVersion: 1,
			MFARequired:       true,
		},
	}
	sessions := &sessionStub{}
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	issuer := &mfaIssuerStub{token: challenge, expiresAt: now.Add(5 * time.Minute)}
	service, err := auth.NewLoginService(
		store,
		sessions,
		auth.WithLoginClock(func() time.Time { return now }),
		auth.WithMFAChallengeIssuer(issuer),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLoginHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"email":"alice@example.com","password":"correct-password","tenant":"acme"}`)
	request := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response mfaRequiredResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.MFARequired || response.ChallengeToken != challenge.Reveal() || !response.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("response=%+v", response)
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Fatal("login HTTP created a session before MFA")
	}
}

func TestMFACompletionHandlerReturnsSession(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	sealed, err := secretCipher.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := auth.NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	membershipID := uuid.MustNew()
	store := &mfaStoreStub{
		challengeFound: true,
		challenge: auth.MFAChallengeRecord{
			SubjectID:         uuid.MustNew(),
			MembershipID:      membershipID,
			CredentialVersion: 1,
			SecretDigest:      sha256.Sum256(secret),
			SecretCiphertext:  sealed,
			LastCounter:       -1,
			ExpiresAt:         now.Add(5 * time.Minute),
			SessionExpiresAt:  now.Add(time.Hour),
		},
		consumeResult:     true,
		consumeMembership: membershipID,
	}
	sessions := &sessionStub{token: sessionToken}
	service, err := auth.NewMFAService(store, sessions, secretCipher, auth.WithMFAClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	auditor := &securityAuditorStub{}
	handler, err := NewAuditedMFACompletionHandler(service, auditor)
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(secret, uint64(now.Unix()/totpPeriodSeconds))
	body, err := json.Marshal(mfaCompletionRequest{ChallengeToken: challenge.Reveal(), Code: code})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/mfa", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "192.0.2.10:4242"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", recorder.Header().Get("Cache-Control"))
	}
	var response loginResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != sessionToken.Reveal() || !response.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("response=%+v", response)
	}
	if len(auditor.events) != 1 || auditor.events[0].Kind != auth.SecurityMFAChallengeSucceeded || auditor.events[0].Outcome != auth.SecurityOutcomeSucceeded {
		t.Fatalf("audit events=%+v", auditor.events)
	}
}

func TestTOTPEnrollmentHandlerRequiresExplicitStepUp(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewMFAService(&mfaStoreStub{}, &sessionStub{}, secretCipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTOTPEnrollmentHandler(service, "Forge", nil); err == nil {
		t.Fatal("nil enrollment authorizer was accepted")
	}

	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	authorized := false
	handler, err := NewTOTPEnrollmentHandler(service, "Forge", auth.MFAEnrollmentAuthorizerFunc(func(_ context.Context, got auth.Principal) error {
		authorized = true
		if got.SubjectID() != principal.SubjectID() {
			t.Fatalf("subject=%s", got.SubjectID())
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/mfa/totp/enroll", nil)
	request = request.WithContext(withPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !authorized {
		t.Fatalf("status=%d authorized=%v body=%s", recorder.Code, authorized, recorder.Body.String())
	}
	var response totpEnrollmentResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Secret == "" || response.ProvisioningURI == "" {
		t.Fatalf("response=%+v", response)
	}
}

func TestTOTPConfirmationHandlerReturnsBackupCodesOnce(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	sealed, err := secretCipher.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	subjectID := uuid.MustNew()
	store := &mfaStoreStub{
		pendingFound:  true,
		confirmResult: true,
		pending: auth.TOTPSecretRecord{
			SubjectID:        subjectID,
			SecretDigest:     sha256.Sum256(secret),
			SecretCiphertext: sealed,
			ExpiresAt:        now.Add(5 * time.Minute),
		},
	}
	service, err := auth.NewMFAService(store, &sessionStub{}, secretCipher, auth.WithMFAClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewTOTPConfirmationHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(subjectID, tenant)
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(secret, uint64(now.Unix()/totpPeriodSeconds))
	body, err := json.Marshal(totpConfirmationRequest{Code: code})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/mfa/totp/confirm", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(withPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", recorder.Header().Get("Cache-Control"))
	}
	var response totpConfirmationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.BackupCodes) != auth.DefaultMFABackupCodeCount {
		t.Fatalf("backup code count=%d", len(response.BackupCodes))
	}
	for _, backupCode := range response.BackupCodes {
		if _, ok := parseMFABackupCode(backupCode); !ok {
			t.Fatalf("invalid backup code in HTTP response: %q", backupCode)
		}
	}
}

func TestMFALifecycleHandlersRequireStepUp(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store := &mfaStoreStub{disabledResult: true}
	service, err := auth.NewMFAService(store, &sessionStub{}, secretCipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTOTPRotationHandler(service, "Forge", nil); err == nil {
		t.Fatal("nil rotation authorizer was accepted")
	}
	if _, err := NewMFADisableHandler(service, nil); err == nil {
		t.Fatal("nil disable authorizer was accepted")
	}

	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	authorized := 0
	authorizer := auth.MFAChangeAuthorizerFunc(func(_ context.Context, got auth.Principal) error {
		authorized++
		if got.SubjectID() != principal.SubjectID() {
			t.Fatalf("subject=%s", got.SubjectID())
		}
		return nil
	})

	rotation, err := NewTOTPRotationHandler(service, "Forge", authorizer)
	if err != nil {
		t.Fatal(err)
	}
	rotateRequest := httptest.NewRequest(http.MethodPost, "/auth/mfa/totp/rotate", nil)
	rotateRequest = rotateRequest.WithContext(withPrincipal(rotateRequest.Context(), principal))
	rotateRecorder := httptest.NewRecorder()
	rotation.ServeHTTP(rotateRecorder, rotateRequest)
	if rotateRecorder.Code != http.StatusOK || !store.rotationStarted {
		t.Fatalf("rotation status=%d started=%v body=%s", rotateRecorder.Code, store.rotationStarted, rotateRecorder.Body.String())
	}

	disable, err := NewMFADisableHandler(service, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	disableRequest := httptest.NewRequest(http.MethodPost, "/auth/mfa/disable", nil)
	disableRequest = disableRequest.WithContext(withPrincipal(disableRequest.Context(), principal))
	disableRecorder := httptest.NewRecorder()
	disable.ServeHTTP(disableRecorder, disableRequest)
	if disableRecorder.Code != http.StatusNoContent {
		t.Fatalf("disable status=%d body=%s", disableRecorder.Code, disableRecorder.Body.String())
	}
	if authorized != 2 {
		t.Fatalf("authorizer calls=%d", authorized)
	}
}

func TestMFADisableHandlerKeepsSuccessWhenOnlyAuditFails(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store := &mfaStoreStub{disabledResult: true}
	service, err := auth.NewMFAService(
		store,
		&sessionStub{},
		secretCipher,
		auth.WithMFAAuditor(&securityAuditorStub{err: errors.New("audit unavailable")}),
	)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewMFADisableHandler(service, auth.MFAChangeAuthorizerFunc(func(context.Context, auth.Principal) error {
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/mfa/disable", nil)
	request = request.WithContext(withPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
