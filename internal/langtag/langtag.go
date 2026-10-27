// Package langtag validates and canonicalizes the subset of BCP 47 locale
// names Forge supports. It is a leaf shared by i18n (CanonicalLocale) and
// config (FORGE_I18N_DEFAULT_LOCALE), so config does not depend on the
// translation runtime (ADR 0052).
package langtag

import (
	"fmt"
	"strings"
)

// MaxLength bounds a locale name: a 3-letter language, a script and a
// 3-digit region ("zzz-Zzzz-999") is the longest name the grammar accepts.
const MaxLength = 12

// Canonical validates name against the subset of BCP 47 Forge
// supports and returns its canonical spelling: a 2 or 3 letter language
// (lowercase), an optional 4 letter script (titlecase) and an optional
// region of 2 letters (uppercase) or 3 digits, separated by "-". "pt-br"
// becomes "pt-BR", "zh-hant-tw" becomes "zh-Hant-TW". "_" separators,
// extensions, variants and private-use subtags are refused, so a valid name
// never contains a path separator, a dot or anything else a file name could
// misuse.
func Canonical(name string) (string, error) {
	if name == "" || len(name) > MaxLength {
		return "", fmt.Errorf("i18n: invalid locale %q: expected a language such as en or pt-BR", truncate(name))
	}
	parts := strings.Split(name, "-")
	if len(parts) > 3 || !isLetters(parts[0]) || len(parts[0]) < 2 || len(parts[0]) > 3 {
		return "", fmt.Errorf("i18n: invalid locale %q: expected a language such as en or pt-BR", truncate(name))
	}
	canonical := []string{strings.ToLower(parts[0])}
	rest := parts[1:]
	if len(rest) > 0 && len(rest[0]) == 4 && isLetters(rest[0]) {
		canonical = append(canonical, strings.ToUpper(rest[0][:1])+strings.ToLower(rest[0][1:]))
		rest = rest[1:]
	}
	if len(rest) > 0 {
		region := rest[0]
		switch {
		case len(region) == 2 && isLetters(region):
			canonical = append(canonical, strings.ToUpper(region))
		case len(region) == 3 && isDigits(region):
			canonical = append(canonical, region)
		default:
			return "", fmt.Errorf("i18n: invalid locale %q: the region must be 2 letters or 3 digits", truncate(name))
		}
		rest = rest[1:]
	}
	if len(rest) > 0 {
		return "", fmt.Errorf("i18n: invalid locale %q: expected language[-Script][-REGION]", truncate(name))
	}
	return strings.Join(canonical, "-"), nil
}

func isLetters(s string) bool {
	for i := range len(s) {
		c := s[i] | 0x20
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return s != ""
}

func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// truncate keeps error messages short when the input came from a request.
func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}
