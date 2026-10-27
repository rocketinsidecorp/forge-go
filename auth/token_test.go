package auth_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
)

func TestTokenRoundTripAndEntropyFailure(t *testing.T) {
	t.Parallel()
	token, err := auth.NewTokenWithEntropy(bytes.NewReader(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.ParseToken(token.Reveal())
	if err != nil || parsed.Digest() != token.Digest() {
		t.Fatalf("ParseToken()=(%v, %v)", parsed, err)
	}
	if _, err := auth.ParseToken(token.Reveal() + "="); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("non-canonical token error=%v", err)
	}
	if _, err := auth.NewTokenWithEntropy(bytes.NewReader(nil)); err == nil {
		t.Fatal("expected entropy failure")
	}
	if formatted := fmt.Sprintf("%v %#v", token, token); formatted != "[REDACTED] auth.Token([REDACTED])" {
		t.Fatalf("token formatting is not redacted: %q", formatted)
	}
}
