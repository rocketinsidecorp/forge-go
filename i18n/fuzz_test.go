package i18n

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// fuzzTranslator is a small catalog for negotiation fuzzing: a default
// locale, a regional variant, a script variant and a 3-digit region.
func fuzzTranslator(tb testing.TB) *Translator {
	tb.Helper()
	translator, err := Load(fstest.MapFS{
		"en.json":         {Data: []byte(`{"en": {"hello": "Hello"}}`)},
		"pt-BR.json":      {Data: []byte(`{"pt-BR": {"hello": "Olá"}}`)},
		"zh-Hant-TW.json": {Data: []byte(`{"zh-Hant-TW": {"hello": "你好"}}`)},
		"es-419.json":     {Data: []byte(`{"es-419": {"hello": "Hola"}}`)},
	}, Options{})
	if err != nil {
		tb.Fatal(err)
	}
	return translator
}

// FuzzParseAcceptLanguage checks the Accept-Language parser and the
// negotiation built on it: bounded output, well-formed ranges sorted by
// quality, and a negotiated locale that is always an available one.
func FuzzParseAcceptLanguage(f *testing.F) {
	for _, seed := range []string{
		"",
		"*",
		"pt-BR,pt;q=0.9,en;q=0.8",
		"zh-Hant-TW-x-private;q=1.000, en;q=0.001",
		"en;q=0, *;q=0.5",
		"en;q=1.0001, fr;q=01, de;q=0.",
		"EN-us ; Q = 0.5 ;foo=bar, es-419",
		strings.Repeat("a,", 40),
		"x-klingon, i-default, 1en",
	} {
		f.Add(seed)
	}
	translator := fuzzTranslator(f)
	locales := translator.Locales()
	f.Fuzz(func(t *testing.T, header string) {
		ranges := parseAcceptLanguage(header)
		if len(header) > maxAcceptLanguageBytes && ranges != nil {
			t.Fatalf("a %d-byte header was parsed", len(header))
		}
		if len(ranges) > maxAcceptLanguageEntries {
			t.Fatalf("%d ranges, more than %d", len(ranges), maxAcceptLanguageEntries)
		}
		for i, entry := range ranges {
			if !languageTag(entry.tag) {
				t.Fatalf("range %q is not a language range", entry.tag)
			}
			if entry.quality < 1 || entry.quality > 1000 {
				t.Fatalf("range %q has quality %d", entry.tag, entry.quality)
			}
			if i > 0 && ranges[i-1].quality < entry.quality {
				t.Fatalf("ranges are not sorted by quality: %+v", ranges)
			}
		}
		tags := ParseAcceptLanguage(header)
		if len(tags) != len(ranges) {
			t.Fatalf("ParseAcceptLanguage = %q, ranges = %+v", tags, ranges)
		}
		locale, ok := translator.matchAcceptLanguage(header)
		if ok != (locale != "") || (ok && !slices.Contains(locales, locale)) {
			t.Fatalf("matchAcceptLanguage(%q) = %q, %v; available %q", header, locale, ok, locales)
		}
		request, err := http.NewRequest(http.MethodGet, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept-Language", header)
		if negotiated := translator.Negotiate(request, NegotiationOptions{}); !slices.Contains(locales, negotiated) {
			t.Fatalf("Negotiate = %q, not an available locale", negotiated)
		}
	})
}

// FuzzCanonicalLocale checks that a canonical locale only changes case, is
// stable, never holds a file-name metacharacter, and names its own file.
func FuzzCanonicalLocale(f *testing.F) {
	for _, seed := range []string{"en", "pt-br", "ZH-HANT-tw", "es-419", "en_US", "../en", "en.json", "sr-Latn-RS-x", "a"} {
		f.Add(seed)
	}
	translator := fuzzTranslator(f)
	f.Fuzz(func(t *testing.T, name string) {
		canonical, err := CanonicalLocale(name)
		if err != nil {
			if canonical != "" {
				t.Fatalf("error with a result %q", canonical)
			}
			if len(err.Error()) > 200+len("i18n: invalid locale") {
				t.Fatalf("error message quotes too much: %d bytes", len(err.Error()))
			}
			return
		}
		if !strings.EqualFold(canonical, name) || len(canonical) > maxLocaleLength {
			t.Fatalf("CanonicalLocale(%q) = %q", name, canonical)
		}
		if strings.ContainsAny(canonical, "/\\.:%\x00 ") {
			t.Fatalf("canonical locale %q holds a file-name metacharacter", canonical)
		}
		if again, err := CanonicalLocale(canonical); err != nil || again != canonical {
			t.Fatalf("not stable: %q -> %q -> %q, %v", name, canonical, again, err)
		}
		if locale, err := FileLocale(canonical + ".json"); err != nil || locale != canonical {
			t.Fatalf("FileLocale(%q) = %q, %v", canonical+".json", locale, err)
		}
		for _, parent := range parents(canonical) {
			if again, err := CanonicalLocale(parent); err != nil || again != parent {
				t.Fatalf("parent %q of %q is not canonical", parent, canonical)
			}
		}
		if matched := translator.Match(name); !slices.Contains(translator.Locales(), matched) {
			t.Fatalf("Match(%q) = %q, not an available locale", name, matched)
		}
	})
}

// fuzzMarker is an argument value that must never reach HTML unescaped.
const fuzzMarker = "<i\x01>"

// FuzzLoadCatalog loads a fuzzed translation file next to a valid default
// catalog. Loading either fails with an error or yields a catalog whose every
// key translates, as text and as HTML, without panicking.
func FuzzLoadCatalog(f *testing.F) {
	for _, seed := range []struct{ name, data string }{
		{"en.json", `{"en": {"greeting": "Hello, %{name}!", "inbox": {"one": "%{count} message", "other": "%{count} messages"}}}`},
		{"pt-BR.json", `{"pt-BR": {"x_html": "<b>%{name}</b>", "list": ["a", null], "n": 1.5, "b": true}}`},
		{"posts.en.json", `{"en": {"posts": {"title": "Posts %{count} %{"}}}`},
		{"en.json", `{"en": {"a": 1, "a": 2}}`},
		{"en.json", `{"en": {"a": {"b": {"c": {"d": {"e": {"f": {"g": {"h": {"i": {"j": {"k": {"l": {"m": {"n": {"o": {"p": {"q": "deep"}}}}}}}}}}}}}}}}}}`},
		{"de.json", `{"de": {"other": "x", "one": 1}}`},
		{"fr.json", `{"fr": null}`},
		{"en.json", `{"en": {}} {}`},
		{"EN.json", `{"en": {}}`},
		{"x.y.en.json", `[`},
	} {
		f.Add(seed.name, []byte(seed.data))
	}
	base := []byte(`{"en": {"hello": "Hello"}}`)
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		// The catalog reads only the root directory: a file name never holds
		// a separator, and fstest.MapFS needs valid paths.
		if !fs.ValidPath(name) || strings.Contains(name, "/") {
			return
		}
		fsys := fstest.MapFS{name: {Data: data}}
		if name != "base.en.json" {
			fsys["base.en.json"] = &fstest.MapFile{Data: base}
		}
		translator, err := Load(fsys, Options{ShowMissing: len(data)%2 == 0})
		if err != nil {
			if translator != nil {
				t.Fatal("Load returned a translator with an error")
			}
			return
		}
		for _, locale := range translator.Locales() {
			keys := translator.Keys(locale)
			if len(keys) > MaxKeysPerLocale {
				t.Fatalf("locale %s has %d keys", locale, len(keys))
			}
			for _, key := range keys {
				if len(key) > MaxKeyLength {
					t.Fatalf("key %q is longer than %d", key, MaxKeyLength)
				}
				_ = translator.Translate(locale, key)
				_ = translator.Translate(locale, key, "count", len(data), "name", "<i>")
				_ = translator.Translate(locale, key, "count", "1.50", "name")
				_ = translator.HTML(locale, key, "name", template.HTML("<b>"), "count", -1)
				// An interpolated string is escaped even in an _html key.
				html := translator.HTML(locale, key, "name", fuzzMarker, "count", 2)
				if strings.Contains(string(html), fuzzMarker) && !bytes.Contains(data, []byte(fuzzMarker)) {
					t.Fatalf("HTML(%s, %s) = %q: an argument was not escaped", locale, key, html)
				}
				if !IsHTMLKey(key) && strings.Contains(string(html), "<") {
					t.Fatalf("HTML(%s, %s) = %q: text was not escaped", locale, key, html)
				}
				if forms, ok := translator.PluralForms(locale, key); ok && !slices.Contains(forms, Other) {
					t.Fatalf("plural %s.%s lacks an %q form: %v", locale, key, Other, forms)
				}
			}
		}
	})
}
