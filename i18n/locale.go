package i18n

import (
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/langtag"
)

// maxLocaleLength bounds a locale name: a 3-letter language, a script and a
// 3-digit region ("zzz-Zzzz-999") is the longest name the grammar accepts.
const maxLocaleLength = langtag.MaxLength

// CanonicalLocale validates name against the subset of BCP 47 Forge
// supports and returns its canonical spelling: a 2 or 3 letter language
// (lowercase), an optional 4 letter script (titlecase) and an optional
// region of 2 letters (uppercase) or 3 digits, separated by "-". "pt-br"
// becomes "pt-BR", "zh-hant-tw" becomes "zh-Hant-TW". "_" separators,
// extensions, variants and private-use subtags are refused, so a valid name
// never contains a path separator, a dot or anything else a file name could
// misuse.
func CanonicalLocale(name string) (string, error) {
	return langtag.Canonical(name)
}

// parents returns the fallback chain of a canonical locale, most specific
// first: "zh-Hant-TW" gives zh-Hant-TW, zh-Hant, zh.
func parents(locale string) []string {
	chain := []string{locale}
	for {
		cut := strings.LastIndexByte(locale, '-')
		if cut < 0 {
			return chain
		}
		locale = locale[:cut]
		chain = append(chain, locale)
	}
}

// language is the language subtag of a canonical locale.
func language(locale string) string {
	if cut := strings.IndexByte(locale, '-'); cut >= 0 {
		return locale[:cut]
	}
	return locale
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
