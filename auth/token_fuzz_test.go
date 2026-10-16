package auth_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
)

func FuzzParseToken(f *testing.F) {
	token, err := auth.NewToken()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(token.Reveal())
	f.Add("")
	f.Add("not-a-token")
	f.Fuzz(func(t *testing.T, value string) {
		parsed, err := auth.ParseToken(value)
		if err == nil && parsed.Reveal() != value {
			t.Fatalf("non-canonical token: %q -> %q", value, parsed.Reveal())
		}
	})
}
