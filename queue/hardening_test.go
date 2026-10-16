package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestSanitizeErrorRedactsLongPayloadStrings(t *testing.T) {
	raw := make([]byte, 300)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	long := hex.EncodeToString(raw) // 600 bytes
	payload, _ := json.Marshal(map[string]string{"blob": long})
	for name, message := range map[string]string{
		"whole":        "upstream rejected " + long,
		"prefix only":  "token starts " + long[:80] + "... truncated by the library",
		"suffix only":  "...tail " + long[len(long)-90:],
		"middle":       "...mid " + long[200:300] + " ...",
		"short middle": "...mid " + long[250:290] + " ...",
	} {
		got := sanitizeError(errors.New(message), payload)
		for i := 0; i+windowSize <= len(long); i++ {
			if strings.Contains(got, long[i:i+windowSize]) {
				t.Errorf("%s: %d bytes of the payload string survived: %.120q", name, windowSize, got)
				break
			}
		}
	}
	// Text that merely resembles it stays readable.
	if got := sanitizeError(errors.New("unrelated failure: connection reset"), payload); !strings.Contains(got, "connection reset") {
		t.Errorf("unrelated text damaged: %q", got)
	}
}

func TestSanitizeErrorRedactsEveryStringOfALargePayloadDeterministically(t *testing.T) {
	fields := map[string]string{}
	for i := range 2000 {
		fields[fmt.Sprintf("k%04d", i)] = fmt.Sprintf("leafvalue-%04d-%s", i, uuid.MustNew())
	}
	fields["zz"] = "TOPSECRETVALUE-1234"
	fields["zzz"] = strings.Repeat("L", 700) + "-LONGSECRET"
	payload, _ := json.Marshal(fields)
	for range 40 {
		got := sanitizeError(errors.New("x TOPSECRETVALUE-1234 and "+strings.Repeat("L", 700)+"-LONGSECRET and leafvalue-0007-"), payload)
		for _, secret := range []string{"TOPSECRETVALUE", "LONGSECRET", "LLLLLLLLLL"} {
			if strings.Contains(got, secret) {
				t.Fatalf("%q survived redaction: %.120q", secret, got)
			}
		}
	}
}

func TestSanitizeErrorKeepsLongerValuesWhenThePayloadHasMoreStringsThanTheBound(t *testing.T) {
	fields := map[string]string{}
	for i := range maxNeedles + 500 {
		fields[fmt.Sprintf("k%06d", i)] = fmt.Sprintf("v%07d", i) // 8 bytes each
	}
	fields["secret"] = "the-longest-value-in-the-payload"
	payload, _ := json.Marshal(fields)
	got := sanitizeError(errors.New("oops the-longest-value-in-the-payload"), payload)
	if strings.Contains(got, "the-longest-value") {
		t.Fatalf("the longest value was dropped before the shorter ones: %q", got)
	}
}

func TestSanitizeErrorStripsTerminalInjection(t *testing.T) {
	for name, r := range map[string]rune{
		"C1 CSI":      '\u009b',
		"C1 NEL":      '\u0085',
		"RLO":         '\u202e',
		"LRE":         '\u202a',
		"isolate":     '\u2066',
		"LRM":         '\u200e',
		"RLM":         '\u200f',
		"ALM":         '\u061c',
		"line sep":    '\u2028',
		"BOM":         '\ufeff',
		"ESC":         '\x1b',
		"DEL":         '\x7f',
		"replacement": '\ufffd',
	} {
		got := sanitizeError(errors.New("bad "+string(r)+"31mred"), nil)
		if strings.ContainsRune(got, r) {
			t.Errorf("%s (U+%04X) survives sanitizing: %q", name, r, got)
		}
		if !strings.Contains(got, "bad ") {
			t.Errorf("%s: lost surrounding text: %q", name, got)
		}
		if err := validateKey("key" + string(r)); err == nil {
			t.Errorf("%s (U+%04X): idempotency key accepted", name, r)
		}
	}
	if err := validateKey("chave-ção-✓ 日本語"); err != nil {
		t.Errorf("printable Unicode rejected: %v", err)
	}
}

func TestEscapeUnsafe(t *testing.T) {
	got := EscapeUnsafe("a\u009b2Jb\u202ec\x1bd\xffe")
	want := `a\u009b2Jb\u202ec\u001bd\xffe`
	if got != want {
		t.Errorf("EscapeUnsafe = %q, want %q", got, want)
	}
	if EscapeUnsafe("plain ação 日本") != "plain ação 日本" {
		t.Error("printable text must pass unchanged")
	}
	if !utf8.ValidString(EscapeUnsafe("\xff\xfe")) {
		t.Error("escaped text must be valid UTF-8")
	}
}

type typedNilError struct{ message *string }

func (e *typedNilError) Error() string { return *e.message }

func TestSanitizeErrorSurvivesAnErrorMethodThatPanics(t *testing.T) {
	var e *typedNilError
	got := sanitizeError(error(e), []byte(`{"a":"b"}`))
	if got != "*queue.typedNilError: error text unavailable" {
		t.Errorf("sanitizeError = %q", got)
	}
}

func TestRunAtMustFallWithinSupportedYears(t *testing.T) {
	job := NewJob{ID: uuid.MustNew(), Kind: "k", Queue: "q", Payload: json.RawMessage(`{}`), MaxAttempts: 1}
	for _, year := range []int{1000, 2026, 9999} {
		job.RunAt = time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC)
		if err := job.Validate(); err != nil {
			t.Errorf("year %d rejected: %v", year, err)
		}
	}
	for _, year := range []int{999, 10000, 1, 12345} {
		job.RunAt = time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC)
		if err := job.Validate(); err == nil {
			t.Errorf("NewJob.Validate accepted year %d", year)
		}
	}
	r := NewRegistry()
	if err := Register(r, "k", func(context.Context, struct{}) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(r, "k", struct{}{}, RunAt(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))); err == nil {
		t.Error("RunAt(year 10000) accepted")
	}
	if _, err := Prepare(r, "k", struct{}{}, RunAt(time.Time{})); err == nil {
		t.Error("RunAt(zero time) accepted")
	}
}
