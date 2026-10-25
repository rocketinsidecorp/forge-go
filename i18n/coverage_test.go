package i18n_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

func TestNewOperandsAcceptsEveryNumberType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value any
		want  i18n.Operands
	}{
		{"int", int(-3), i18n.Operands{N: 3, I: 3}},
		{"int8", int8(-4), i18n.Operands{N: 4, I: 4}},
		{"int16", int16(5), i18n.Operands{N: 5, I: 5}},
		{"int32", int32(-6), i18n.Operands{N: 6, I: 6}},
		{"int64", int64(7), i18n.Operands{N: 7, I: 7}},
		{"min int64 saturates", int64(math.MinInt64), i18n.Operands{N: float64(math.MaxInt64), I: math.MaxInt64}},
		{"uint", uint(8), i18n.Operands{N: 8, I: 8}},
		{"uint8", uint8(9), i18n.Operands{N: 9, I: 9}},
		{"uint16", uint16(10), i18n.Operands{N: 10, I: 10}},
		{"uint32", uint32(11), i18n.Operands{N: 11, I: 11}},
		{"uint64", uint64(12), i18n.Operands{N: 12, I: 12}},
		{"uint64 beyond int64 saturates", uint64(math.MaxUint64), i18n.Operands{N: float64(math.MaxInt64), I: math.MaxInt64}},
		{"float32", float32(1.5), i18n.Operands{N: 1.5, I: 1, V: 1}},
		{"float64", float64(-2.25), i18n.Operands{N: 2.25, I: 2, V: 2}},
	}
	for _, tc := range tests {
		got, ok := i18n.NewOperands(tc.value)
		if !ok || got != tc.want {
			t.Errorf("%s: NewOperands(%v) = %+v, %v; want %+v", tc.name, tc.value, got, ok, tc.want)
		}
	}
	for _, notNumber := range []any{"3", nil, true, []int{1}} {
		if _, ok := i18n.NewOperands(notNumber); ok {
			t.Errorf("NewOperands(%v) accepted a non-number", notNumber)
		}
	}
	if got, ok := i18n.NewOperands(math.NaN()); !ok || !math.IsNaN(got.N) || got.V != 1 {
		t.Errorf("NaN = %+v, %v", got, ok)
	}
}

func TestCanonicalLocaleRejectsMalformedNames(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"en-US-GB":   "expected language[-Script][-REGION]",
		"en-USA":     "the region must be 2 letters or 3 digits",
		"e":          "expected a language",
		"":           "expected a language",
		"en-US-x-pr": "expected a language",
	} {
		if got, err := i18n.CanonicalLocale(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CanonicalLocale(%q) = %q, %v; want an error mentioning %q", name, got, err, want)
		}
	}
	for name, want := range map[string]string{"EN": "en", "zh-hant-tw": "zh-Hant-TW", "es-419": "es-419", "pt-br": "pt-BR"} {
		if got, err := i18n.CanonicalLocale(name); err != nil || got != want {
			t.Errorf("CanonicalLocale(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

// --- loading ---

type failingFile struct{ fs.File }

func (failingFile) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

// brokenFS lists en.json but cannot open or read it, or cannot list at all.
type brokenFS struct {
	fstest.MapFS
	mode string
}

func (b brokenFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if b.mode == "list" {
		return nil, errors.New("directory unreadable")
	}
	return b.MapFS.ReadDir(name)
}

func (b brokenFS) Open(name string) (fs.File, error) {
	switch {
	case b.mode == "open" && name == "en.json":
		return nil, errors.New("file unreadable")
	case b.mode == "read" && name == "en.json":
		file, err := b.MapFS.Open(name)
		return failingFile{file}, err
	}
	return b.MapFS.Open(name)
}

func TestLoadReportsFileSystemFailures(t *testing.T) {
	t.Parallel()
	good := fstest.MapFS{"en.json": {Data: []byte(`{"en":{"a":"b"}}`)}}
	for mode, want := range map[string]string{
		"list": "read translations: directory unreadable",
		"open": "open en.json: file unreadable",
		"read": "read en.json: disk on fire",
	} {
		if _, err := i18n.Load(brokenFS{MapFS: good, mode: mode}, i18n.Options{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", mode, err, want)
		}
	}
	if _, err := i18n.Load(nil, i18n.Options{}); err == nil || !strings.Contains(err.Error(), "filesystem is required") {
		t.Errorf("nil filesystem: %v", err)
	}
}

func TestLoadEnforcesItsLimits(t *testing.T) {
	t.Parallel()
	t.Run("too many files", func(t *testing.T) {
		t.Parallel()
		files := fstest.MapFS{}
		for i := 0; i <= i18n.MaxFiles; i++ {
			files[fmt.Sprintf("s%04d.en.json", i)] = &fstest.MapFile{Data: []byte(`{"en":{}}`)}
		}
		if _, err := i18n.Load(files, i18n.Options{}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d translation files", i18n.MaxFiles)) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("too many keys in a locale", func(t *testing.T) {
		t.Parallel()
		var body strings.Builder
		body.WriteString(`{"en":{`)
		for i := 0; i <= i18n.MaxKeysPerLocale; i++ {
			if i > 0 {
				body.WriteByte(',')
			}
			fmt.Fprintf(&body, `"k%d":"v"`, i)
		}
		body.WriteString(`}}`)
		files := fstest.MapFS{"en.json": {Data: []byte(body.String())}}
		if _, err := i18n.Load(files, i18n.Options{}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("locale en has more than %d keys", i18n.MaxKeysPerLocale)) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLoadRejectsMalformedFiles(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("k", i18n.MaxKeyLength+1)
	for name, tc := range map[string]struct{ file, want string }{
		"truncated":              {`{"en": {"a": `, "invalid JSON"},
		"missing comma":          {`{"en": {"a": "b" "c": "d"}}`, "invalid JSON"},
		"list with a number":     {`{"en": {"l": ["a", 1]}}`, `lists may hold only strings and null`},
		"key beyond the maximum": {`{"en": {"` + long + `": "x"}}`, "is longer than"},
		"null value":             {`{"en": {"a": null}}`, "is null"},
		"duplicate key":          {`{"en": {"a": "x", "a": "y"}}`, "duplicate key"},
		"plural without other":   {`{"en": {"n": {"one": "x"}}}`, `need an "other" form`},
		"plural that is a list":  {`{"en": {"n": {"one": ["x"], "other": "y"}}}`, "must be a string"},
		"trailing data":          {`{"en": {}} {}`, "unexpected data"},
		"wrong locale key":       {`{"pt": {"a": "x"}}`, `must be "en"`},
	} {
		files := fstest.MapFS{"en.json": {Data: []byte(tc.file)}}
		if _, err := i18n.Load(files, i18n.Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestMergeConflictsNameTheFileThatDefinedTheKeyFirst(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		first, second map[string]string
		want          string
	}{
		"leaf and leaf": {map[string]string{"en.json": `{"en":{"a":"x"}}`}, map[string]string{"posts.en.json": `{"en":{"a":"y"}}`}, `key "a" is defined in both en.json and posts.en.json`},
		"leaf over a group": {
			map[string]string{"en.json": `{"en":{"a":{"b":"x"}}}`}, map[string]string{"posts.en.json": `{"en":{"a":"y"}}`},
			`key "a" is defined in both en.json and posts.en.json`,
		},
		"group over a leaf": {
			map[string]string{"en.json": `{"en":{"a":"y"}}`}, map[string]string{"posts.en.json": `{"en":{"a":{"b":"x"}}}`},
			`key "a" is defined in both en.json and posts.en.json`,
		},
		"a group that only looks like plural forms": {
			map[string]string{"en.json": `{"en":{"a":{"other":"x","extra":"y"}}}`}, map[string]string{"posts.en.json": `{"en":{"a":{"other":"z"}}}`},
			`key "a" is defined in both`,
		},
	} {
		files := fstest.MapFS{}
		for file, data := range tc.first {
			files[file] = &fstest.MapFile{Data: []byte(data)}
		}
		for file, data := range tc.second {
			files[file] = &fstest.MapFile{Data: []byte(data)}
		}
		if _, err := i18n.Load(files, i18n.Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// --- translator ---

func coverageTranslator(t *testing.T, files map[string]string, opts i18n.Options) *i18n.Translator {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, data := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}
	translator, err := i18n.Load(fsys, opts)
	if err != nil {
		t.Fatal(err)
	}
	return translator
}

func TestTranslatorQueriesOfAnUnknownLocaleAreEmpty(t *testing.T) {
	t.Parallel()
	translator := coverageTranslator(t, map[string]string{"en.json": `{"en":{"a":"b","n":{"one":"1","other":"n"}}}`}, i18n.Options{})
	if keys := translator.Keys("xx"); keys != nil {
		t.Errorf("Keys = %v", keys)
	}
	if forms, ok := translator.PluralForms("xx", "n"); ok || forms != nil {
		t.Errorf("PluralForms = %v, %v", forms, ok)
	}
	if translator.Has("xx", "a") {
		t.Error("Has reports a key of an unknown locale")
	}
	if forms, ok := translator.PluralForms("en", "n"); !ok || len(forms) != 2 {
		t.Errorf("PluralForms of a plural key = %v, %v", forms, ok)
	}
	if _, ok := translator.PluralForms("en", "a"); ok {
		t.Error("a text key has no plural forms")
	}
}

func TestInvalidKeysAndArgumentsAreReportedAndNeverPanic(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	translator := coverageTranslator(t, map[string]string{"en.json": `{"en":{"greet":"Hi %{name} %{bad name} %{unterminated"}}`},
		i18n.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	huge := strings.Repeat("k", 2*i18n.MaxKeyLength)
	if got := translator.Translate("en", ""); got != "" {
		t.Errorf("an empty key is humanized to %q", got)
	}
	if got := translator.Translate("en", huge); got != strings.ToUpper(huge[:1])+huge[1:] {
		t.Errorf("a huge key is humanized to a string of %d bytes", len(got))
	}
	if strings.Contains(logs.String(), huge) {
		t.Error("the warning holds the whole oversized key")
	}
	// An argument name that is not an identifier is skipped and reported.
	got := translator.Translate("en", "greet", "", "x", "name", "Ana")
	if got != "Hi Ana %{bad name} %{unterminated" {
		t.Errorf("greeting = %q", got)
	}
	if !strings.Contains(logs.String(), "not a name") {
		t.Errorf("the bad argument was not reported: %s", logs.String())
	}
}

func TestContextHelpersWithoutAndWithATranslator(t *testing.T) {
	t.Parallel()
	translator := coverageTranslator(t, map[string]string{"en.json": `{"en":{"hello_html":"<b>%{name}</b>","plain":"Plain"}}`}, i18n.Options{})
	ctx := context.Background()
	if i18n.NewContext(ctx, nil, "en") != ctx {
		t.Error("a nil translator leaves the context unchanged")
	}
	var nilCtx context.Context
	if tr, locale, ok := i18n.FromContext(nilCtx); tr != nil || locale != "" || ok {
		t.Errorf("FromContext(nil) = %v, %q, %v", tr, locale, ok)
	}
	if got := i18n.T(ctx, "user.full_name"); got != "Full name" {
		t.Errorf("T without a translator = %q", got)
	}
	if got := i18n.HTML(ctx, "a.<b>"); !strings.Contains(string(got), "&lt;") {
		t.Errorf("HTML without a translator = %q", got)
	}
	bound := i18n.NewContext(ctx, translator, "en")
	if got := i18n.HTML(bound, "hello_html", "name", "<i>"); got != "<b>&lt;i&gt;</b>" {
		t.Errorf("HTML = %q", got)
	}
	if got := i18n.T(bound, "plain"); got != "Plain" {
		t.Errorf("T = %q", got)
	}
}

// --- formatting ---

func TestFormatNumberEdgeCases(t *testing.T) {
	t.Parallel()
	translator := coverageTranslator(t, map[string]string{"en.json": `{"en":{"number":{"format":{"separator":".","delimiter":",","precision":99}}}}`}, i18n.Options{})
	tests := []struct {
		name      string
		value     any
		precision int
		want      string
		ok        bool
	}{
		{"precision is capped at 20", 1.5, 40, "1." + strings.Repeat("5", 1) + strings.Repeat("0", 19), true},
		{"an invalid default precision falls back to 2", 1234.5, -1, "1,234.50", true},
		{"float32", float32(0.5), 1, "0.5", true},
		{"NaN", math.NaN(), 2, "NaN", true},
		{"positive infinity", math.Inf(1), 2, "+Inf", true},
		{"negative infinity", math.Inf(-1), 2, "-Inf", true},
		{"integer with decimals", 1234567, 2, "1,234,567.00", true},
		{"not a number", "abc", 2, "abc", false},
	}
	for _, tc := range tests {
		got, ok := translator.FormatNumber("en", tc.value, tc.precision)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: FormatNumber = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFormatTimeDirectives(t *testing.T) {
	t.Parallel()
	patterns := []struct{ pattern, want string }{
		{"%e|%-e", " 5|5"},
		{"%I|%-I", "12|12"},
		{"%d|%-d", "05|5"},
		{"%j", "064"},
		{"%Q|%-Q", "%Q|%-Q"},
		{"%%|100%", "%|100%"},
		{"%B", "March"}, // the catalog's month_names is not a list: the built-in names serve
		{"%H:%M:%S %z", "00:07:09 +0000"},
		{"%y%Z", "26UTC"},
	}
	formats := make([]string, len(patterns))
	for i, p := range patterns {
		formats[i] = fmt.Sprintf("%q:%q", fmt.Sprintf("f%d", i), p.pattern)
	}
	translator := coverageTranslator(t, map[string]string{
		"en.json": `{"en":{"date":{"month_names":"not a list"},"time":{"formats":{` + strings.Join(formats, ",") + `,"pm":"%I %-I %p"}}}}`,
	}, i18n.Options{})
	early := time.Date(2026, 3, 5, 0, 7, 9, 0, time.UTC)
	for i, p := range patterns {
		if got := translator.FormatTime("en", early, fmt.Sprintf("f%d", i)); got != p.want {
			t.Errorf("%q = %q, want %q", p.pattern, got, p.want)
		}
	}
	noon := time.Date(2026, 3, 5, 15, 7, 9, 0, time.UTC)
	if got := translator.FormatTime("en", noon, "pm"); got != "03 3 PM" {
		t.Errorf("afternoon = %q", got)
	}
}

// --- negotiation ---

func TestMatchChoosesAVariantOfTheRequestedLanguage(t *testing.T) {
	t.Parallel()
	translator := coverageTranslator(t, map[string]string{
		"pt-BR.json": `{"pt-BR":{"a":"x"}}`, "en.json": `{"en":{"a":"y"}}`, "es-MX.json": `{"es-MX":{"a":"z"}}`,
	}, i18n.Options{DefaultLocale: "pt-BR"})
	for requested, want := range map[string]string{
		"pt-PT": "pt-BR", // the default locale's language
		"pt":    "pt-BR",
		"es":    "es-MX", // another available variant of the language
		"es-AR": "es-MX",
		"fr":    "pt-BR", // nothing: the default
		"x":     "pt-BR", // not a locale at all
	} {
		if got := translator.Match(requested); got != want {
			t.Errorf("Match(%q) = %q, want %q", requested, got, want)
		}
	}
}

func TestNegotiationHonorsACustomCookieAndStripsTheWholePrefix(t *testing.T) {
	t.Parallel()
	translator := coverageTranslator(t, map[string]string{"en.json": `{"en":{"a":"x"}}`, "pt-BR.json": `{"pt-BR":{"a":"y"}}`}, i18n.Options{})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "pt-BR"})
	if got := translator.Negotiate(request, i18n.NegotiationOptions{Cookie: "lang"}); got != "pt-BR" {
		t.Errorf("custom cookie: %q", got)
	}
	if got := translator.Negotiate(request, i18n.NegotiationOptions{}); got != "en" {
		t.Errorf("the default cookie name does not see it: %q", got)
	}

	middleware, err := translator.Middleware(i18n.NegotiationOptions{PathPrefix: true})
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	handler := middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.URL.Path + "|" + r.URL.RawPath }))
	prefixOnly := httptest.NewRequest(http.MethodGet, "/pt-BR", nil)
	prefixOnly.URL.RawPath = "/pt-BR"
	handler.ServeHTTP(httptest.NewRecorder(), prefixOnly)
	if seen != "/|/" {
		t.Errorf("a bare locale segment leaves %q", seen)
	}

	for _, name := range []string{strings.Repeat("c", 65), "bad name", "semi;colon", ""} {
		_, err := translator.Middleware(i18n.NegotiationOptions{Cookie: name})
		if (name == "") != (err == nil) {
			t.Errorf("cookie name %q: err = %v", name, err)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("Middleware accepted a nil next handler")
		}
	}()
	middleware(nil)
}
