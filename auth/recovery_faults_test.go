package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errInjected }

func TestRecoveryTokenEntropyFailuresAndRedaction(t *testing.T) {
	t.Parallel()
	if _, err := newRecoveryTokenWithEntropy(nil); err == nil {
		t.Error("a nil entropy source was accepted")
	}
	if _, err := newRecoveryTokenWithEntropy(failingReader{}); !errors.Is(err, errInjected) {
		t.Errorf("failing entropy: %v", err)
	}
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	if got := token.LogValue().String(); got != "[REDACTED]" {
		t.Errorf("LogValue=%q", got)
	}
}

func TestRecoveryServiceConstructionValidatesItsOptions(t *testing.T) {
	t.Parallel()
	store, sender := &recoveryStoreStub{}, &recoverySenderStub{}
	if _, err := NewRecoveryService(nil, sender); err == nil {
		t.Error("a nil store was accepted")
	}
	if _, err := NewRecoveryService(store, nil); err == nil {
		t.Error("a nil sender was accepted")
	}
	for name, option := range map[string]RecoveryOption{
		"nil option":    nil,
		"short ttl":     WithRecoveryTTL(time.Second),
		"long ttl":      WithRecoveryTTL(48 * time.Hour),
		"nil clock":     WithRecoveryClock(nil),
		"nil throttler": WithRecoveryThrottler(nil),
		"nil auditor":   WithRecoveryAuditor(nil),
		"nil dispatch":  WithRecoveryDispatcher(nil),
		"nil logger":    WithRecoveryLogger(nil),
		"long response": WithRecoveryResponseTime(time.Minute),
		"neg response":  WithRecoveryResponseTime(-time.Second),
	} {
		if _, err := NewRecoveryService(store, sender, option); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	var sent PasswordRecoveryMessage
	fn := RecoverySenderFunc(func(_ context.Context, message PasswordRecoveryMessage) error {
		sent = message
		return errInjected
	})
	if err := fn.SendPasswordRecovery(context.Background(), PasswordRecoveryMessage{Email: "a@example.com"}); !errors.Is(err, errInjected) || sent.Email != "a@example.com" {
		t.Errorf("RecoverySenderFunc: sent=%+v err=%v", sent, err)
	}
	if got := (&RecoveryThrottledError{}).Error(); got != ErrRecoveryThrottled.Error() {
		t.Errorf("throttled message=%q", got)
	}
	if err := (&RecoveryDeliveryError{}).Unwrap(); !errors.Is(err, ErrRecoveryDelivery) {
		t.Errorf("a delivery error without a cause must still be ErrRecoveryDelivery: %v", err)
	}
}

func TestRecoveryServiceFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newService := func(store RecoveryStore, options ...RecoveryOption) *RecoveryService {
		options = append([]RecoveryOption{WithRecoveryResponseTime(0)}, options...)
		service, err := NewRecoveryService(store, &recoverySenderStub{}, options...)
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	request := RecoveryRequest{Email: "alice@example.com", TenantSlug: "acme", Source: "192.0.2.1"}

	// The throttler failing is an error, whoever asks.
	service := newService(knownRecoveryStore(), WithRecoveryThrottler(failingThrottler{attemptErr: errInjected}))
	if err := service.Request(ctx, request); !errors.Is(err, errInjected) {
		t.Errorf("request throttler failure: %v", err)
	}
	if err := service.Reset(ctx, PasswordReset{Token: mustRecoveryToken(t).Reveal(), NewPassword: "a long enough password"}); !errors.Is(err, errInjected) {
		t.Errorf("reset throttler failure: %v", err)
	}

	// Process: a job that names no account is a no-op; store failures and an
	// invalid identity are errors that issue nothing.
	store := knownRecoveryStore()
	service = newService(store)
	if err := service.Process(ctx, RecoveryJob{Email: "not an email", TenantSlug: "acme"}); err != nil {
		t.Errorf("malformed job: %v", err)
	}
	if store.lookupEmail != "" {
		t.Error("a malformed job reached the store")
	}
	store.lookupErr = errInjected
	if err := service.Process(ctx, RecoveryJob{Email: "alice@example.com", TenantSlug: "acme"}); !errors.Is(err, errInjected) {
		t.Errorf("lookup failure: %v", err)
	}
	store.lookupErr = nil
	for name, identity := range map[string]RecoveryIdentity{
		"nil subject": {Email: "alice@example.com"},
		"no email":    {SubjectID: uuid.MustNew()},
	} {
		store.identity = identity
		if err := service.Process(ctx, RecoveryJob{Email: "alice@example.com", TenantSlug: "acme"}); err == nil || !strings.Contains(err.Error(), "invalid identity") {
			t.Errorf("%s: %v", name, err)
		}
		if store.createdDigest != (Digest{}) {
			t.Errorf("%s: a token was issued", name)
		}
	}
	store = knownRecoveryStore()
	store.createErr = errInjected
	service = newService(store)
	if err := service.Process(ctx, RecoveryJob{Email: "alice@example.com", TenantSlug: "acme"}); !errors.Is(err, errInjected) {
		t.Errorf("create failure: %v", err)
	}

	// A dispatcher failing for another reason than a full queue is returned.
	service = newService(knownRecoveryStore(), WithRecoveryDispatcher(&dispatcherStub{err: errInjected}))
	if err := service.Request(ctx, request); !errors.Is(err, errInjected) {
		t.Errorf("dispatch failure: %v", err)
	}
}

func mustRecoveryToken(t *testing.T) RecoveryToken {
	t.Helper()
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRecoveryResetFailsClosedOnStoreAndPasswordErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := mustRecoveryToken(t)
	auditor := &securityAuditorStub{}
	newService := func(store RecoveryStore) *RecoveryService {
		service, err := NewRecoveryService(store, &recoverySenderStub{}, WithRecoveryResponseTime(0), WithRecoveryAuditor(auditor))
		if err != nil {
			t.Fatal(err)
		}
		return service
	}

	// The usability check failing is an error, and an unusable token is
	// invalid, audited as denied, without hashing the password.
	checker := &tokenCheckerStore{err: errInjected}
	if err := newService(checker).Reset(ctx, PasswordReset{Token: token.Reveal(), NewPassword: "a long enough password"}); !errors.Is(err, errInjected) {
		t.Errorf("checker failure: %v", err)
	}

	// An empty or oversized password is a typed refusal, audited as denied.
	store := &recoveryStoreStub{consumeResult: true}
	for name, password := range map[string]string{"empty": "", "too long": strings.Repeat("x", maxPasswordBytes+1)} {
		auditor.events = nil
		err := newService(store).Reset(ctx, PasswordReset{Token: token.Reveal(), NewPassword: password})
		if !errors.Is(err, ErrPasswordInvalid) {
			t.Errorf("%s password: %v", name, err)
		}
		if !hasEvent(auditor, SecurityPasswordReset, SecurityOutcomeDenied) {
			t.Errorf("%s password: no denied event: %+v", name, auditor.events)
		}
		if store.consumedDigest != (Digest{}) {
			t.Errorf("%s password: the token was consumed", name)
		}
	}

	// The store failing to consume is an error.
	store = &recoveryStoreStub{consumeErr: errInjected}
	if err := newService(store).Reset(ctx, PasswordReset{Token: token.Reveal(), NewPassword: "a long enough password"}); !errors.Is(err, errInjected) {
		t.Errorf("consume failure: %v", err)
	}
	// A malformed token is invalid before any throttling or store work.
	if err := newService(store).Reset(ctx, PasswordReset{Token: "nope", NewPassword: "a long enough password"}); !errors.Is(err, ErrRecoveryInvalid) {
		t.Errorf("malformed token: %v", err)
	}
}
