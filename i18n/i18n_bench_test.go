package i18n_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

// BenchmarkI18nLookup is scenario "i18n_lookup" in docs/BENCHMARKS.md:
// Translator.Translate on a loaded catalog.
//
//	simple         a key without arguments
//	interpolation  a key with one %{name} argument
//	plural_one     the "one" form chosen by a count of 1
//	plural_other   the "other" form chosen by a count of 42
//	fallback       a key that pt-BR lacks, answered by its parent locale pt
func BenchmarkI18nLookup(b *testing.B) {
	translator, err := i18n.Load(catalog(nil), i18n.Options{})
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct {
		name, locale, key string
		args              []any
		want              string
	}{
		{"simple", "en", "users.welcome", nil, "Welcome back"},
		{"interpolation", "en", "greeting", []any{"name", "Ada"}, "Hello, Ada!"},
		{"plural_one", "en", "inbox", []any{"count", 1}, "1 message"},
		{"plural_other", "en", "inbox", []any{"count", 42}, "42 messages"},
		{"fallback", "pt-BR", "only_pt", nil, "Só em português"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			if got := translator.Translate(c.locale, c.key, c.args...); got != c.want {
				b.Fatalf("Translate = %q, want %q", got, c.want)
			}
			b.ReportAllocs()
			for b.Loop() {
				_ = translator.Translate(c.locale, c.key, c.args...)
			}
		})
	}
}
