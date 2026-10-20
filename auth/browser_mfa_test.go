package auth

import (
	"context"
	"encoding/base32"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

// mfaAccount is one account of memoryMFAAccounts.
type mfaAccount struct {
	login       string
	hash        string
	pending     *TOTPSecretRecord
	factor      *TOTPSecretRecord
	backupCodes map[Digest]bool // digest -> used
	revocations int
}

// memoryMFAAccounts implements the MFA stores and SubjectPasswordStore for
// several accounts with the semantics auth/postgres has.
type memoryMFAAccounts struct {
	mu       sync.Mutex
	accounts map[uuid.UUID]*mfaAccount
}

func (m *memoryMFAAccounts) account(subjectID uuid.UUID) *mfaAccount { return m.accounts[subjectID] }

func (m *memoryMFAAccounts) LookupSubjectPassword(_ context.Context, subjectID uuid.UUID) (SubjectPassword, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil {
		return SubjectPassword{}, false, nil
	}
	return SubjectPassword{SubjectID: subjectID, Login: account.login, PasswordHash: account.hash}, true, nil
}

func (m *memoryMFAAccounts) begin(subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time, rotation bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil {
		return errors.New("no such account")
	}
	if rotation && account.factor == nil {
		return ErrMFANotEnabled
	}
	if !rotation && account.factor != nil {
		return ErrMFAAlreadyEnabled
	}
	account.pending = &TOTPSecretRecord{SubjectID: subjectID, SecretDigest: digest, SecretCiphertext: ciphertext, ExpiresAt: expiresAt}
	return nil
}

func (m *memoryMFAAccounts) BeginTOTPEnrollment(_ context.Context, subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time) error {
	return m.begin(subjectID, digest, ciphertext, expiresAt, false)
}

func (m *memoryMFAAccounts) BeginTOTPRotation(_ context.Context, subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time) error {
	return m.begin(subjectID, digest, ciphertext, expiresAt, true)
}

func (m *memoryMFAAccounts) LoadPendingTOTP(_ context.Context, subjectID uuid.UUID) (TOTPSecretRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.pending == nil {
		return TOTPSecretRecord{}, false, nil
	}
	return *account.pending, true, nil
}

func (m *memoryMFAAccounts) confirm(subjectID uuid.UUID, digest Digest, counter int64, backup []Digest, rotation bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.pending == nil || account.pending.SecretDigest != digest || (account.factor != nil) != rotation {
		return false, nil
	}
	factor := *account.pending
	factor.LastCounter = counter
	account.factor, account.pending = &factor, nil
	account.backupCodes = map[Digest]bool{}
	for _, code := range backup {
		account.backupCodes[code] = false
	}
	account.revocations++
	return true, nil
}

func (m *memoryMFAAccounts) ConfirmTOTPEnrollment(_ context.Context, subjectID uuid.UUID, digest Digest, counter int64, backup []Digest) (bool, error) {
	return m.confirm(subjectID, digest, counter, backup, false)
}

func (m *memoryMFAAccounts) ConfirmTOTPRotation(_ context.Context, subjectID uuid.UUID, digest Digest, counter int64, backup []Digest) (bool, error) {
	return m.confirm(subjectID, digest, counter, backup, true)
}

func (m *memoryMFAAccounts) DisableMFA(_ context.Context, subjectID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return false, nil
	}
	account.factor, account.pending, account.backupCodes = nil, nil, nil
	account.revocations++
	return true, nil
}

func (m *memoryMFAAccounts) LoadMFAStatus(_ context.Context, subjectID uuid.UUID) (MFAStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return MFAStatus{}, nil
	}
	status := MFAStatus{Enabled: true}
	for _, used := range account.backupCodes {
		if !used {
			status.BackupCodesRemaining++
		}
	}
	return status, nil
}

func (m *memoryMFAAccounts) LoadActiveTOTP(_ context.Context, subjectID uuid.UUID) (TOTPSecretRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return TOTPSecretRecord{}, false, nil
	}
	return *account.factor, true, nil
}

func (m *memoryMFAAccounts) AdvanceTOTPCounter(_ context.Context, subjectID uuid.UUID, digest Digest, counter int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil || account.factor.SecretDigest != digest || counter <= account.factor.LastCounter {
		return false, nil
	}
	account.factor.LastCounter = counter
	return true, nil
}

func (m *memoryMFAAccounts) ConsumeMFABackupCode(_ context.Context, subjectID uuid.UUID, digest Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return false, nil
	}
	used, exists := account.backupCodes[digest]
	if !exists || used {
		return false, nil
	}
	account.backupCodes[digest] = true
	return true, nil
}

func (m *memoryMFAAccounts) ReplaceMFABackupCodes(_ context.Context, subjectID uuid.UUID, digests []Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return false, nil
	}
	account.backupCodes = map[Digest]bool{}
	for _, digest := range digests {
		account.backupCodes[digest] = false
	}
	return true, nil
}

func (*memoryMFAAccounts) CreateMFAChallenge(context.Context, Digest, uuid.UUID, int64, time.Time, time.Time) error {
	return errors.New("not used")
}

func (*memoryMFAAccounts) LoadMFAChallenge(context.Context, Digest) (MFAChallengeRecord, bool, error) {
	return MFAChallengeRecord{}, false, errors.New("not used")
}

func (*memoryMFAAccounts) ConsumeMFAChallenge(context.Context, Digest, Digest, int64) (uuid.UUID, bool, error) {
	return uuid.UUID{}, false, errors.New("not used")
}

func (*memoryMFAAccounts) ConsumeMFABackupChallenge(context.Context, Digest, Digest) (uuid.UUID, bool, error) {
	return uuid.UUID{}, false, errors.New("not used")
}

// mfaSettingsFixture drives the Browser two-factor settings for two
// accounts. The X-Subject header picks the signed-in account, as
// Middleware.AuthenticateBrowser would from the session cookie.
type mfaSettingsFixture struct {
	t        *testing.T
	accounts *memoryMFAAccounts
	ada, bob uuid.UUID
	now      time.Time
	clockMu  sync.Mutex
	events   []SecurityEvent
	app      http.Handler
}

const mfaTestPassword = "correct horse battery staple"

func newMFASettingsFixture(t *testing.T, throttle LoginThrottleConfig) *mfaSettingsFixture {
	t.Helper()
	hash, err := HashPassword(mfaTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	f := &mfaSettingsFixture{
		t:   t,
		ada: uuid.MustNew(), bob: uuid.MustNew(),
		now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
	}
	f.accounts = &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{
		f.ada: {login: "ada@example.com", hash: hash},
		f.bob: {login: "bob@example.com", hash: hash},
	}}
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	var auditMu sync.Mutex
	auditor := SecurityAuditorFunc(func(_ context.Context, event SecurityEvent) error {
		auditMu.Lock()
		defer auditMu.Unlock()
		f.events = append(f.events, event)
		return nil
	})
	throttler, err := NewMemoryLoginThrottler(throttle)
	if err != nil {
		t.Fatal(err)
	}
	mfa, err := NewMFAService(f.accounts, &sessionStub{}, cipher, WithMFAClock(f.clock), WithMFAAuditor(auditor), WithMFAThrottler(throttler))
	if err != nil {
		t.Fatal(err)
	}
	login := browserLogin(t, knownIdentity(t, mfaTestPassword, false), &sessionStub{})
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: mfa, Passwords: f.accounts})
	if err != nil {
		t.Fatal(err)
	}
	f.app = mfaSettingsApp(t, browser)
	return f
}

func (f *mfaSettingsFixture) clock() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.now
}

func (f *mfaSettingsFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = f.now.Add(d)
}

// code is the authenticator code of base32Key now.
func (f *mfaSettingsFixture) code(base32Key string) string {
	f.t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(base32Key)
	if err != nil {
		f.t.Fatal(err)
	}
	return totpCode(secret, uint64(f.clock().Unix()/totpPeriodSeconds))
}

func (f *mfaSettingsFixture) post(subject uuid.UUID, path string, form url.Values, withToken bool) *httptest.ResponseRecorder {
	f.t.Helper()
	token, err := csrf.NewToken()
	if err != nil {
		f.t.Fatal(err)
	}
	values := url.Values{}
	for key, value := range form {
		values[key] = value
	}
	if withToken {
		values.Set(csrf.FormField, token)
	}
	request := httptest.NewRequest(http.MethodPost, browserSite+path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	if subject != (uuid.UUID{}) {
		request.Header.Set("X-Subject", subject.String())
	}
	recorder := httptest.NewRecorder()
	f.app.ServeHTTP(recorder, request)
	return recorder
}

func (f *mfaSettingsFixture) status(subject uuid.UUID) MFAStatus {
	status, err := f.accounts.LoadMFAStatus(context.Background(), subject)
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

func (f *mfaSettingsFixture) deniedEvents(kind SecurityEventKind) int {
	count := 0
	for _, event := range f.events {
		if event.Kind == kind && event.Outcome == SecurityOutcomeDenied {
			count++
		}
	}
	return count
}

// enable turns MFA on for subject and returns the key and backup codes.
func (f *mfaSettingsFixture) enable(subject uuid.UUID) (string, []string) {
	f.t.Helper()
	begun := f.post(subject, "/mfa", url.Values{"password": {mfaTestPassword}}, true)
	key := begun.Header().Get("X-Key")
	if begun.Code != http.StatusOK || key == "" {
		f.t.Fatalf("begin status=%d error=%q", begun.Code, begun.Header().Get("X-Error"))
	}
	confirmed := f.post(subject, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {f.code(key)}}, true)
	if confirmed.Code != http.StatusOK {
		f.t.Fatalf("confirm status=%d error=%q", confirmed.Code, confirmed.Header().Get("X-Error"))
	}
	f.advance(totpPeriodSeconds * time.Second)
	return key, strings.Split(confirmed.Header().Get("X-Backup-Codes"), ",")
}

// mfaSettingsApp mounts the settings methods as a generated adapter does,
// writing outcomes to headers: X-Key for a key shown, X-Backup-Codes for
// codes shown, X-Error otherwise.
func mfaSettingsApp(t *testing.T, browser *Browser) http.Handler {
	t.Helper()
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	fail := func(w http.ResponseWriter, r *http.Request, err error) {
		var pending *MFAPendingCodeError
		if errors.As(err, &pending) {
			w.Header().Set("X-Key", pending.Enrollment.Secret.Reveal())
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("X-Error", err.Error())
		web.Error(w, r, err)
	}
	proof := func(r *http.Request, codeField string) MFAStepUp {
		return MFAStepUp{Password: r.PostFormValue("password"), Code: r.PostFormValue(codeField)}
	}
	showKey := func(w http.ResponseWriter, r *http.Request, enrollment TOTPEnrollment, err error) {
		if err != nil {
			fail(w, r, err)
			return
		}
		w.Header().Set("X-Key", enrollment.Secret.Reveal())
		w.WriteHeader(http.StatusOK)
	}
	showCodes := func(w http.ResponseWriter, r *http.Request, codes []MFABackupCode, err error) {
		if err != nil {
			fail(w, r, err)
			return
		}
		shown := make([]string, 0, len(codes))
		for _, code := range codes {
			shown = append(shown, code.Reveal())
		}
		w.Header().Set("X-Backup-Codes", strings.Join(shown, ","))
		w.WriteHeader(http.StatusOK)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mfa", func(w http.ResponseWriter, r *http.Request) {
		enrollment, err := browser.BeginMFAEnrollment(w, r, "Forge", proof(r, "code"))
		showKey(w, r, enrollment, err)
	})
	mux.HandleFunc("POST /mfa/confirm", func(w http.ResponseWriter, r *http.Request) {
		codes, err := browser.ConfirmMFAEnrollment(w, r, "Forge", proof(r, "current_code"), r.PostFormValue("code"))
		showCodes(w, r, codes, err)
	})
	mux.HandleFunc("POST /mfa/rotation", func(w http.ResponseWriter, r *http.Request) {
		enrollment, err := browser.BeginMFARotation(w, r, "Forge", proof(r, "code"))
		showKey(w, r, enrollment, err)
	})
	mux.HandleFunc("POST /mfa/rotation/confirm", func(w http.ResponseWriter, r *http.Request) {
		codes, err := browser.ConfirmMFARotation(w, r, "Forge", proof(r, "current_code"), r.PostFormValue("code"))
		showCodes(w, r, codes, err)
	})
	mux.HandleFunc("POST /mfa/backup_codes", func(w http.ResponseWriter, r *http.Request) {
		codes, err := browser.RegenerateMFABackupCodes(w, r, proof(r, "code"))
		showCodes(w, r, codes, err)
	})
	mux.HandleFunc("DELETE /mfa", func(w http.ResponseWriter, r *http.Request) {
		if err := browser.DisableMFA(w, r, proof(r, "code")); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	signedIn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subject, err := uuid.Parse(r.Header.Get("X-Subject")); err == nil {
			principal, err := NewPrincipal(subject, tenant)
			if err != nil {
				t.Fatal(err)
			}
			r = r.WithContext(withPrincipal(r.Context(), principal))
		}
		mux.ServeHTTP(w, r)
	})
	return web.MethodOverride(protection.Handler(signedIn))
}

func TestBrowserMFAEnrollmentNeedsThePasswordAtBothSteps(t *testing.T) {
	t.Parallel()
	f := newMFASettingsFixture(t, DefaultMFAThrottleConfig())

	if anonymous := f.post(uuid.UUID{}, "/mfa", url.Values{"password": {mfaTestPassword}}, true); anonymous.Code != http.StatusUnauthorized ||
		anonymous.Header().Get("X-Key") != "" {
		t.Fatalf("signed out: status=%d", anonymous.Code)
	}
	if forged := f.post(f.ada, "/mfa", url.Values{"password": {mfaTestPassword}}, false); forged.Code != http.StatusForbidden ||
		forged.Header().Get("X-Key") != "" {
		t.Fatalf("missing CSRF token: status=%d", forged.Code)
	}
	if wrong := f.post(f.ada, "/mfa", url.Values{"password": {"wrong password"}}, true); wrong.Code != http.StatusUnauthorized ||
		wrong.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() || wrong.Header().Get("X-Key") != "" {
		t.Fatalf("wrong password: status=%d error=%q", wrong.Code, wrong.Header().Get("X-Error"))
	}
	if f.accounts.account(f.ada).pending != nil {
		t.Fatal("a refused step-up left a pending key")
	}

	begun := f.post(f.ada, "/mfa", url.Values{"password": {mfaTestPassword}}, true)
	key := begun.Header().Get("X-Key")
	if begun.Code != http.StatusOK || key == "" || begun.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("begin status=%d headers=%v", begun.Code, begun.Header())
	}
	code := f.code(key)

	// A stolen session (no password) can neither see the key again nor
	// try codes.
	for name, form := range map[string]url.Values{
		"no password":    {"code": {code}},
		"wrong password": {"password": {"wrong password"}, "code": {code}},
	} {
		refused := f.post(f.ada, "/mfa/confirm", form, true)
		if refused.Code != http.StatusUnauthorized || refused.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() ||
			refused.Header().Get("X-Key") != "" || strings.Contains(refused.Body.String(), key) {
			t.Fatalf("%s: status=%d headers=%v", name, refused.Code, refused.Header())
		}
	}
	if forged := f.post(f.ada, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {code}}, false); forged.Code != http.StatusForbidden {
		t.Fatalf("confirm without CSRF token: status=%d", forged.Code)
	}
	if f.status(f.ada).Enabled {
		t.Fatal("MFA turned on without re-authentication")
	}

	// Another account's code does not confirm Ada's key.
	bobKey, _ := f.enable(f.bob)
	if crossed := f.post(f.ada, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {f.code(bobKey)}}, true); crossed.Code != http.StatusUnprocessableEntity ||
		crossed.Header().Get("X-Key") != key {
		t.Fatalf("wrong code: status=%d, want the pending key shown again", crossed.Code)
	}

	confirmed := f.post(f.ada, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {f.code(key)}}, true)
	codes := strings.Split(confirmed.Header().Get("X-Backup-Codes"), ",")
	if confirmed.Code != http.StatusOK || len(codes) != DefaultMFABackupCodeCount {
		t.Fatalf("confirm status=%d codes=%d error=%q", confirmed.Code, len(codes), confirmed.Header().Get("X-Error"))
	}
	if session := responseCookie(confirmed, SessionCookieName); session == nil || session.MaxAge >= 0 {
		t.Fatal("confirmation must clear the session cookie: the store revoked every session")
	}
	if status := f.status(f.ada); !status.Enabled || status.BackupCodesRemaining != DefaultMFABackupCodeCount {
		t.Fatalf("status=%+v", status)
	}
	if again := f.post(f.ada, "/mfa", url.Values{"password": {mfaTestPassword}}, true); again.Header().Get("X-Error") != ErrMFAAlreadyEnabled.Error() {
		t.Fatalf("second enrollment: status=%d error=%q", again.Code, again.Header().Get("X-Error"))
	}
	if f.deniedEvents(SecurityMFAEnrollmentStarted) != 1 || f.deniedEvents(SecurityMFAEnabled) != 3 {
		t.Fatalf("denied audit events: start=%d enable=%d", f.deniedEvents(SecurityMFAEnrollmentStarted), f.deniedEvents(SecurityMFAEnabled))
	}
	for _, event := range f.events {
		if event.SubjectID != f.ada && event.SubjectID != f.bob {
			t.Fatalf("audit event without the account: %+v", event)
		}
	}
}

func TestBrowserMFAChangesNeedThePasswordAndAFreshCode(t *testing.T) {
	t.Parallel()
	// Generous limits: this test makes many refused attempts on purpose.
	generous := DefaultMFAThrottleConfig()
	generous.AccountLimit, generous.SourceLimit = 1000, 1000
	f := newMFASettingsFixture(t, generous)
	key, backup := f.enable(f.ada)
	bobKey, _ := f.enable(f.bob)

	stepUp := func(code string) url.Values { return url.Values{"password": {mfaTestPassword}, "code": {code}} }
	for name, form := range map[string]url.Values{
		"no code":        {"password": {mfaTestPassword}},
		"no password":    {"code": {f.code(key)}},
		"other account":  stepUp(f.code(bobKey)),
		"malformed":      stepUp("12345"),
		"wrong password": {"password": {"wrong password"}, "code": {backup[0]}},
		"wrong code":     stepUp("000000"),
	} {
		for _, path := range []string{"/mfa/rotation", "/mfa/backup_codes"} {
			refused := f.post(f.ada, path, form, true)
			if refused.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() || refused.Header().Get("X-Key") != "" || refused.Header().Get("X-Backup-Codes") != "" {
				t.Fatalf("%s %s: status=%d headers=%v", name, path, refused.Code, refused.Header())
			}
		}
	}
	if f.accounts.account(f.ada).backupCodes[mustBackupDigest(t, backup[0])] {
		t.Fatal("a refused password must not use up the backup code")
	}

	// A code works once: replaying it is refused.
	code := f.code(key)
	regenerated := f.post(f.ada, "/mfa/backup_codes", stepUp(code), true)
	fresh := strings.Split(regenerated.Header().Get("X-Backup-Codes"), ",")
	if regenerated.Code != http.StatusOK || len(fresh) != DefaultMFABackupCodeCount {
		t.Fatalf("regenerate status=%d error=%q", regenerated.Code, regenerated.Header().Get("X-Error"))
	}
	if responseCookie(regenerated, SessionCookieName) != nil {
		t.Fatal("new backup codes must not sign the account out")
	}
	if replayed := f.post(f.ada, "/mfa/backup_codes", stepUp(code), true); replayed.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() {
		t.Fatalf("replayed code: status=%d", replayed.Code)
	}
	// The old backup codes stopped working; a new one works once.
	if old := f.post(f.ada, "/mfa/backup_codes", stepUp(backup[1]), true); old.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() {
		t.Fatalf("old backup code: status=%d", old.Code)
	}

	// Rotation: begin with a backup code, confirm with password, a current
	// code and a code of the new key.
	begun := f.post(f.ada, "/mfa/rotation", stepUp(fresh[0]), true)
	newKey := begun.Header().Get("X-Key")
	if begun.Code != http.StatusOK || newKey == "" || newKey == key {
		t.Fatalf("rotation status=%d error=%q", begun.Code, begun.Header().Get("X-Error"))
	}
	f.advance(totpPeriodSeconds * time.Second)
	for name, form := range map[string]url.Values{
		"no current code":   {"password": {mfaTestPassword}, "code": {f.code(newKey)}},
		"used current code": {"password": {mfaTestPassword}, "current_code": {fresh[0]}, "code": {f.code(newKey)}},
		"no password":       {"current_code": {fresh[1]}, "code": {f.code(newKey)}},
	} {
		refused := f.post(f.ada, "/mfa/rotation/confirm", form, true)
		if refused.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() || refused.Header().Get("X-Key") != "" {
			t.Fatalf("%s: status=%d headers=%v", name, refused.Code, refused.Header())
		}
	}
	if status := f.status(f.ada); !status.Enabled {
		t.Fatal("the old factor must stay active until the rotation is confirmed")
	}
	rotated := f.post(f.ada, "/mfa/rotation/confirm", url.Values{
		"password": {mfaTestPassword}, "current_code": {f.code(key)}, "code": {f.code(newKey)},
	}, true)
	if rotated.Code != http.StatusOK || len(strings.Split(rotated.Header().Get("X-Backup-Codes"), ",")) != DefaultMFABackupCodeCount {
		t.Fatalf("rotation confirm status=%d error=%q", rotated.Code, rotated.Header().Get("X-Error"))
	}
	if session := responseCookie(rotated, SessionCookieName); session == nil || session.MaxAge >= 0 {
		t.Fatal("rotation must clear the session cookie")
	}

	// Disable: the old key no longer counts; the new one does.
	f.advance(totpPeriodSeconds * time.Second)
	if stale := f.post(f.ada, "/mfa", url.Values{"_method": {"DELETE"}, "password": {mfaTestPassword}, "code": {f.code(key)}}, true); stale.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() {
		t.Fatalf("old key after rotation: status=%d", stale.Code)
	}
	disabled := f.post(f.ada, "/mfa", url.Values{"_method": {"DELETE"}, "password": {mfaTestPassword}, "code": {f.code(newKey)}}, true)
	if disabled.Code != http.StatusNoContent || f.status(f.ada).Enabled {
		t.Fatalf("disable status=%d error=%q", disabled.Code, disabled.Header().Get("X-Error"))
	}
	if session := responseCookie(disabled, SessionCookieName); session == nil || session.MaxAge >= 0 {
		t.Fatal("disable must clear the session cookie")
	}
	if !f.status(f.bob).Enabled {
		t.Fatal("another account's factor changed")
	}
}

func TestBrowserMFAStepUpIsThrottled(t *testing.T) {
	t.Parallel()
	f := newMFASettingsFixture(t, DefaultMFAThrottleConfig())
	key, _ := f.enable(f.ada)
	limit := DefaultMFAThrottleConfig().AccountLimit
	for range limit {
		if refused := f.post(f.ada, "/mfa/backup_codes", url.Values{"password": {"wrong password"}, "code": {"000000"}}, true); refused.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() {
			t.Fatalf("status=%d", refused.Code)
		}
	}
	throttled := f.post(f.ada, "/mfa/backup_codes", url.Values{"password": {mfaTestPassword}, "code": {f.code(key)}}, true)
	if throttled.Code != http.StatusTooManyRequests || throttled.Header().Get("Retry-After") == "" || throttled.Header().Get("X-Backup-Codes") != "" {
		t.Fatalf("throttled status=%d headers=%v", throttled.Code, throttled.Header())
	}
	if f.deniedEvents(SecurityMFABackupCodesRegenerated) != limit+1 {
		t.Fatalf("denied events=%d, want %d", f.deniedEvents(SecurityMFABackupCodesRegenerated), limit+1)
	}
	// Wrong codes of a new key count too, even after a right password.
	if other := f.post(f.bob, "/mfa", url.Values{"password": {mfaTestPassword}}, true); other.Code != http.StatusOK {
		t.Fatalf("another account is not throttled: status=%d", other.Code)
	}
	for range limit {
		if wrong := f.post(f.bob, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {"000000"}}, true); wrong.Code != http.StatusUnprocessableEntity {
			t.Fatalf("wrong new code: status=%d", wrong.Code)
		}
	}
	if guessed := f.post(f.bob, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {"111111"}}, true); guessed.Code != http.StatusTooManyRequests {
		t.Fatalf("guessing the new code is not throttled: status=%d", guessed.Code)
	}
}

func TestBrowserMFASettingsFailClosedWithoutPasswords(t *testing.T) {
	t.Parallel()
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	mfa, err := NewMFAService(&memoryMFAAccounts{}, &sessionStub{}, cipher)
	if err != nil {
		t.Fatal(err)
	}
	login := browserLogin(t, knownIdentity(t, mfaTestPassword, false), &sessionStub{})
	browser, err := NewBrowser(BrowserOptions{Login: login, Sessions: &browserRevoker{}, MFA: mfa})
	if err != nil {
		t.Fatal(err)
	}
	if browser.MFASettingsAvailable() {
		t.Fatal("settings need a SubjectPasswordStore")
	}
	app := mfaSettingsApp(t, browser)
	request := func() *httptest.ResponseRecorder {
		return submit(app, "/mfa", url.Values{"password": {mfaTestPassword}}, true)
	}
	if refused := request(); refused.Code != http.StatusServiceUnavailable || refused.Header().Get("X-Error") != ErrMFAUnavailable.Error() {
		t.Fatalf("status=%d error=%q", refused.Code, refused.Header().Get("X-Error"))
	}
}

func mustBackupDigest(t *testing.T, code string) Digest {
	t.Helper()
	digest, ok := parseMFABackupCode(code)
	if !ok {
		t.Fatalf("not a backup code: %q", code)
	}
	return digest
}

func TestTOTPCodeAtMatchesTheVerifier(t *testing.T) {
	t.Parallel()
	secret := []byte("12345678901234567890")
	key := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	at := time.Unix(59, 0)
	code, err := TOTPCodeAt(strings.ToLower(key), at)
	if err != nil || code != "287082" {
		t.Fatalf("code=%q err=%v, want RFC 6238's 287082", code, err)
	}
	if _, ok := matchTOTPCounter(secret, code, at, -1); !ok {
		t.Fatal("the verifier rejects TOTPCodeAt's code")
	}
	for _, bad := range []string{"", "not base32!", "MFRGG"} {
		if _, err := TOTPCodeAt(bad, at); err == nil {
			t.Errorf("TOTPCodeAt(%q) succeeded", bad)
		}
	}
	if _, err := TOTPCodeAt(key, time.Unix(-60, 0)); err == nil {
		t.Error("a time before 1970 was accepted")
	}
}

// TestBrowserMFAPasswordOnlyStepUpKeepsTheCodeCounter: a session that holds
// the password must not reset the code guesses of an account with a factor
// by interleaving password-only step-ups (POST /mfa, POST /mfa/confirm).
func TestBrowserMFAPasswordOnlyStepUpKeepsTheCodeCounter(t *testing.T) {
	t.Parallel()
	for _, passwordOnly := range []string{"/mfa", "/mfa/confirm"} {
		t.Run(strings.TrimPrefix(passwordOnly, "/"), func(t *testing.T) {
			t.Parallel()
			f := newMFASettingsFixture(t, DefaultMFAThrottleConfig())
			f.enable(f.ada)
			wrongCode := url.Values{"_method": {"DELETE"}, "password": {mfaTestPassword}, "code": {"000000"}}
			limit := DefaultMFAThrottleConfig().AccountLimit
			for range limit - 1 {
				if wrong := f.post(f.ada, "/mfa", wrongCode, true); wrong.Header().Get("X-Error") != ErrMFAStepUpInvalid.Error() {
					t.Fatalf("wrong code: status=%d", wrong.Code)
				}
			}
			if interleaved := f.post(f.ada, passwordOnly, url.Values{"password": {mfaTestPassword}, "code": {"000000"}}, true); interleaved.Code == http.StatusTooManyRequests {
				t.Fatalf("the password-only step-up was throttled early: status=%d", interleaved.Code)
			}
			if next := f.post(f.ada, "/mfa", wrongCode, true); next.Code != http.StatusTooManyRequests {
				t.Fatalf("the code counter was reset by a password-only step-up: status=%d error=%q", next.Code, next.Header().Get("X-Error"))
			}
			if !f.status(f.ada).Enabled {
				t.Fatal("MFA was turned off")
			}
		})
	}
}

// TestBrowserMFAEnrollmentConfirmCannotFinishARotation: the password-only
// enrollment confirmation must not install, reveal or test a pending
// rotation key of an account that already has a factor.
func TestBrowserMFAEnrollmentConfirmCannotFinishARotation(t *testing.T) {
	t.Parallel()
	f := newMFASettingsFixture(t, DefaultMFAThrottleConfig())
	key, _ := f.enable(f.ada)
	begun := f.post(f.ada, "/mfa/rotation", url.Values{"password": {mfaTestPassword}, "code": {f.code(key)}}, true)
	newKey := begun.Header().Get("X-Key")
	if begun.Code != http.StatusOK || newKey == "" {
		t.Fatalf("rotation status=%d error=%q", begun.Code, begun.Header().Get("X-Error"))
	}
	for _, code := range []string{"000000", f.code(newKey)} {
		refused := f.post(f.ada, "/mfa/confirm", url.Values{"password": {mfaTestPassword}, "code": {code}}, true)
		if refused.Header().Get("X-Error") != ErrMFAAlreadyEnabled.Error() || refused.Header().Get("X-Key") != "" {
			t.Fatalf("status=%d headers=%v", refused.Code, refused.Header())
		}
	}
	account := f.accounts.account(f.ada)
	if account.pending == nil || account.factor == nil || account.factor.SecretDigest == account.pending.SecretDigest {
		t.Fatal("the rotation was confirmed without a current code")
	}
}
