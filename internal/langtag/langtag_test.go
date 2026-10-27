package langtag_test

import (
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/langtag"
)

func TestCanonicalSpellsEachSubtagInItsCase(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"en":           "en",
		"PT-br":        "pt-BR",
		"zh-hant-tw":   "zh-Hant-TW",
		"es-419":       "es-419",
		"zzz-Zzzz-999": "zzz-Zzzz-999",
	} {
		if got, err := langtag.Canonical(name); err != nil || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestCanonicalRefusesNamesOutsideTheGrammar(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"":                       "expected a language",
		"e":                      "expected a language",
		"en_US":                  "expected a language",
		"../en":                  "expected a language",
		"en-U1":                  "the region must be 2 letters or 3 digits",
		"en-4a1":                 "the region must be 2 letters or 3 digits",
		"en-Latn-US-x":           "expected a language",
		"en-US-x":                "expected language[-Script][-REGION]",
		strings.Repeat("a", 100): "...",
	} {
		if got, err := langtag.Canonical(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Canonical(%q) = %q, %v; want an error mentioning %q", name, got, err, want)
		}
	}
	if len("zzz-Zzzz-999") != langtag.MaxLength {
		t.Errorf("MaxLength = %d, want the longest accepted name", langtag.MaxLength)
	}
}
