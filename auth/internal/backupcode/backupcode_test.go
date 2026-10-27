package backupcode_test

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth/internal/backupcode"
)

func TestGeneratedCodesParseToTheirDigest(t *testing.T) {
	t.Parallel()
	code, digest, err := backupcode.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 29 || strings.Count(code, "-") != 5 {
		t.Fatalf("code %q, want six dash-separated groups", code)
	}
	for _, typed := range []string{code, strings.ToLower(code), " " + strings.ReplaceAll(code, "-", "") + " "} {
		if parsed, ok := backupcode.Parse(typed); !ok || parsed != digest {
			t.Errorf("Parse(%q) = %x, %v; want the code's digest", typed, parsed, ok)
		}
	}
	if fixed, _, err := backupcode.Generate(bytes.NewReader(make([]byte, backupcode.Bytes))); err != nil || fixed != "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA" {
		t.Errorf("zero entropy: %q, %v", fixed, err)
	}
}

func TestParseRefusesWhatIsNotACode(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "AAAA", "AAAA-AAAA-AAAA-AAAA-AAAA-AAA1", "AAAA-AAAA-AAAA-AAAA-AAAA-AAA8", strings.Repeat("A", 25)} {
		if _, ok := backupcode.Parse(value); ok {
			t.Errorf("Parse(%q) accepted", value)
		}
	}
	if _, _, err := backupcode.Generate(bytes.NewReader(nil)); err == nil {
		t.Error("an empty entropy source was accepted")
	}
}
