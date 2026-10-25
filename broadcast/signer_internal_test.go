package broadcast

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// forge builds a token that carries payload under a valid signature, the
// token a compromised or buggy signer holding the key could issue: Verify
// must still refuse what the payload cannot legitimately name.
func forge(t *testing.T, s *Signer, payload string) string {
	t.Helper()
	body := tokenVersion + "." + payload
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(s.key, body))
}

func TestVerifyRefusesAWellSignedTokenWithAPayloadThatMeansNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	signer, err := NewSigner(SignerOptions{Key: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	encode := func(c claims) string {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	future := now.Add(time.Hour).Unix()
	tenant := uuid.MustNew().String()
	valid := encode(claims{Name: "room", Expires: future})
	if _, err := signer.Verify(forge(t, signer, valid)); err != nil {
		t.Fatalf("the control token was refused: %v", err)
	}
	for name, payload := range map[string]string{
		"payload is not base64":          "***not-base64***",
		"payload is not JSON":            base64.RawURLEncoding.EncodeToString([]byte("not json")),
		"global channel with a bad name": encode(claims{Name: "Bad Name!", Expires: future}),
		"tenant that is not a UUID":      encode(claims{Tenant: "not-a-uuid", Name: "room", Expires: future}),
		"tenant channel with a bad name": encode(claims{Tenant: tenant, Name: "Bad Name!", Expires: future}),
		"tenant that is not a UUIDv7":    encode(claims{Tenant: "11111111-1111-4111-8111-111111111111", Name: "room", Expires: future}),
	} {
		if _, err := signer.Verify(forge(t, signer, payload)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: Verify = %v, want ErrInvalidToken", name, err)
		}
	}
	if _, err := signer.Verify(forge(t, signer, encode(claims{Name: "room", Expires: now.Unix()}))); !errors.Is(err, ErrExpiredToken) {
		t.Errorf("a token expiring now: %v, want ErrExpiredToken", err)
	}
}

func TestRandomKeyIsLongEnoughForASigner(t *testing.T) {
	t.Parallel()
	first, err := RandomKey()
	if err != nil || len(first) != MinKeyBytes {
		t.Fatalf("RandomKey = %d bytes, %v", len(first), err)
	}
	second, err := RandomKey()
	if err != nil || string(first) == string(second) {
		t.Fatalf("two keys are equal (err=%v)", err)
	}
	if _, err := NewSigner(SignerOptions{Key: first}); err != nil {
		t.Errorf("a random key was refused: %v", err)
	}
}
