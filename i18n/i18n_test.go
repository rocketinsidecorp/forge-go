package i18n_test

import (
	"bytes"
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

const enJSON = `{"en": {
	"greeting": "Hello, %{name}!",
	"percent": "100%% sure, %{name}",
	"users": {"welcome": "Welcome back", "intro_html": "<strong>%{name}</strong> joined"},
	"inbox": {"zero": "No messages", "one": "%{count} message", "other": "%{count} messages"},
	"only_en": "English only",
	"number": {"format": {"separator": ".", "delimiter": ",", "precision": 2}},
	"date": {"formats": {"long": "%B %-d, %Y"}}
}}`

const ptJSON = `{"pt": {
	"greeting": "Olá, %{name}!",
	"inbox": {"one": "%{count} mensagem", "other": "%{count} mensagens"},
	"only_pt": "Só em português",
	"number": {"format": {"separator": ",", "delimiter": ".", "precision": 2}},
	"date": {
		"formats": {"long": "%-d de %B de %Y"},
		"month_names": [null, "janeiro", "fevereiro", "março", "abril", "maio", "junho", "julho", "agosto", "setembro", "outubro", "novembro", "dezembro"],
		"day_names": ["domingo", "segunda-feira", "terça-feira", "quarta-feira", "quinta-feira", "sexta-feira", "sábado"]
	},
	"time": {"formats": {"short": "%d/%m %H:%M"}}
}}`

const ptBRJSON = `{"pt-BR": {"greeting": "Oi, %{name}!"}}`

func catalog(extra map[string]string) fstest.MapFS {
	files := fstest.MapFS{
		"en.json":       {Data: []byte(enJSON)},
		"pt.json":       {Data: []byte(ptJSON)},
		"pt-BR.json":    {Data: []byte(ptBRJSON)},
		"README.md":     {Data: []byte("ignored")},
		"nested/x.json": {Data: []byte("not read")},
	}
	for name, data := range extra {
		files[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return files
}

func load(t *testing.T, opts i18n.Options) *i18n.Translator {
	t.Helper()
	translator, err := i18n.Load(catalog(nil), opts)
	if err != nil {
		t.Fatal(err)
	}
	return translator
}

func TestCanonicalLocale(t *testing.T) {
	t.Parallel()
	valid := map[string]string{
		"en": "en", "EN": "en", "pt-br": "pt-BR", "pt-BR": "pt-BR", "zh-hant-tw": "zh-Hant-TW",
		"es-419": "es-419", "sr-Latn": "sr-Latn", "fil": "fil",
	}
	for input, want := range valid {
		if got, err := i18n.CanonicalLocale(input); err != nil || got != want {
			t.Errorf("CanonicalLocale(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", "e", "english", "pt_BR", "../en", "en/../../etc/passwd", "en.json", "en-", "-en", "en--US",
		"en-US-x-private", "en-USA", "en-1", "en-Latn-US-extra", "e1", "en-12", "pt-BR\x00", "%2e%2e", "en US",
	} {
		if got, err := i18n.CanonicalLocale(input); err == nil {
			t.Errorf("CanonicalLocale(%q) = %q, want an error", input, got)
		}
	}
}

func TestLoadMergesScopedFilesAndListsKeys(t *testing.T) {
	t.Parallel()
	translator, err := i18n.Load(catalog(map[string]string{"posts.en.json": `{"en": {"posts": {"index": {"title": "Posts"}}}}`}), i18n.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := translator.Locales(); !slices.Equal(got, []string{"en", "pt", "pt-BR"}) {
		t.Fatalf("Locales() = %v", got)
	}
	if translator.DefaultLocale() != "en" {
		t.Fatalf("DefaultLocale() = %q", translator.DefaultLocale())
	}
	keys := translator.Keys("en")
	for _, want := range []string{"posts.index.title", "inbox", "users.intro_html", "date.formats.long"} {
		if !slices.Contains(keys, want) {
			t.Errorf("Keys(en) lacks %s: %v", want, keys)
		}
	}
	if slices.Contains(keys, "inbox.one") {
		t.Error("plural forms must count as one key")
	}
	if !translator.Has("en", "posts.index.title") || translator.Has("pt", "only_en") || translator.Has("en", "users") {
		t.Error("Has reports the wrong keys")
	}
	if forms, ok := translator.PluralForms("pt", "inbox"); !ok || !slices.Equal(forms, []i18n.Category{i18n.One, i18n.Other}) {
		t.Errorf("PluralForms(pt, inbox) = %v, %v", forms, ok)
	}
}

func TestLoadRefusesUnsafeOrAmbiguousFiles(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		files fstest.MapFS
		want  string
	}{
		"traversal in name":     {fstest.MapFS{"en.json": {Data: []byte(`{"en":{}}`)}, "%2e%2e%2fetc.json": {Data: []byte(`{}`)}}, "invalid locale"},
		"underscore locale":     {fstest.MapFS{"pt_BR.json": {Data: []byte(`{"pt_BR":{}}`)}}, "invalid locale"},
		"non canonical name":    {fstest.MapFS{"pt-br.json": {Data: []byte(`{"pt-BR":{}}`)}}, "rename the file to pt-BR.json"},
		"root key mismatch":     {fstest.MapFS{"en.json": {Data: []byte(`{"pt":{"a":"b"}}`)}}, `only top-level key must be "en"`},
		"two root keys":         {fstest.MapFS{"en.json": {Data: []byte(`{"en":{},"pt":{}}`)}}, "only key is the locale"},
		"duplicate key":         {fstest.MapFS{"en.json": {Data: []byte(`{"en":{"a":"1","a":"2"}}`)}}, `duplicate key "a"`},
		"dotted key":            {fstest.MapFS{"en.json": {Data: []byte(`{"en":{"a.b":"1"}}`)}}, "dots separate levels"},
		"null value":            {fstest.MapFS{"en.json": {Data: []byte(`{"en":{"a":null}}`)}}, "is null"},
		"plural without other":  {fstest.MapFS{"en.json": {Data: []byte(`{"en":{"a":{"one":"x"}}}`)}}, `need an "other" form`},
		"invalid json":          {fstest.MapFS{"en.json": {Data: []byte(`{"en":{"a":}}`)}}, "invalid JSON"},
		"trailing data":         {fstest.MapFS{"en.json": {Data: []byte(`{"en":{}} {}`)}}, "after the top-level object"},
		"conflict across files": {fstest.MapFS{"en.json": {Data: []byte(`{"en":{"a":{"b":"1"}}}`)}, "x.en.json": {Data: []byte(`{"en":{"a":{"b":"2"}}}`)}}, `"a.b" is defined in both en.json and x.en.json`},
		"too large":             {fstest.MapFS{"en.json": {Data: append([]byte(`{"en":{"a":"`), bytes.Repeat([]byte("x"), i18n.MaxFileBytes)...)}}, "larger than"},
		"too deep":              {fstest.MapFS{"en.json": {Data: []byte(`{"en":` + strings.Repeat(`{"a":`, i18n.MaxDepth+1) + `"x"` + strings.Repeat("}", i18n.MaxDepth+2))}}, "nesting deeper"},
		"no files":              {fstest.MapFS{"README.md": {Data: []byte("x")}}, "no translation files"},
		"default without file":  {fstest.MapFS{"pt.json": {Data: []byte(`{"pt":{}}`)}}, "default locale en has no translation file"},
		"bad scope":             {fstest.MapFS{"en.json": {Data: []byte(`{"en":{}}`)}, "Posts.en.json": {Data: []byte(`{"en":{}}`)}}, "part before the locale"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := i18n.Load(test.files, i18n.Options{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want it to contain %q", err, test.want)
			}
		})
	}
	if _, err := i18n.Load(catalog(nil), i18n.Options{DefaultLocale: "../../etc"}); err == nil {
		t.Fatal("Load accepted an invalid default locale")
	}
	if _, err := i18n.Load(nil, i18n.Options{}); err == nil {
		t.Fatal("Load accepted a nil filesystem")
	}
}

func TestTranslateInterpolatesAndFallsBackThroughParents(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{ShowMissing: true})
	cases := []struct {
		locale, key string
		args        []any
		want        string
	}{
		{"en", "greeting", []any{"name", "Ana"}, "Hello, Ana!"},
		{"pt-BR", "greeting", []any{"name", "Ana"}, "Oi, Ana!"},
		{"pt-BR", "only_pt", nil, "Só em português"}, // pt-BR -> pt
		{"pt-PT", "greeting", []any{"name", "Ana"}, "Olá, Ana!"},
		{"PT-br", "greeting", []any{"name", "Ana"}, "Oi, Ana!"},
		{"fr", "greeting", []any{"name", "Ana"}, "Hello, Ana!"}, // unknown locale -> default
		{"../x", "greeting", []any{"name", "Ana"}, "Hello, Ana!"},
		{"en", "percent", []any{"name", "Ana"}, "100% sure, Ana"},
		{"en", "greeting", nil, "Hello, %{name}!"}, // a missing value stays visible
		{"en", "inbox", []any{"count", 0}, "No messages"},
		{"en", "inbox", []any{"count", 1}, "1 message"},
		{"en", "inbox", []any{"count", 2}, "2 messages"},
		{"en", "inbox", []any{"count", -1}, "-1 message"},
		{"en", "inbox", []any{"count", 1.5}, "1.5 messages"},
		{"en", "inbox", nil, "%{count} messages"},
		{"pt", "inbox", []any{"count", 0}, "0 mensagem"}, // CLDR pt: 0 and 1 are singular
		{"pt", "inbox", []any{"count", int64(1_000_000)}, "1000000 mensagens"},
		{"pt-BR", "only_en", nil, "translation missing: pt-BR.only_en"},
		{"en", "users", nil, "translation missing: en.users"},
		{"en", "users.welcome.deeper", nil, "translation missing: en.users.welcome.deeper"},
		{"en", "bad key!", nil, "translation missing: en.bad key!"},
	}
	for _, test := range cases {
		if got := translator.Translate(test.locale, test.key, test.args...); got != test.want {
			t.Errorf("Translate(%q, %q, %v) = %q, want %q", test.locale, test.key, test.args, got, test.want)
		}
	}
}

func TestMissingTranslationsInProductionFallBackAndLogOnce(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	translator := load(t, i18n.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if got := translator.Translate("pt-BR", "only_en"); got != "English only" {
		t.Fatalf("default-locale fallback = %q", got)
	}
	if got := translator.Translate("pt-BR", "users.welcome_back_html"); got != "Welcome back" {
		t.Fatalf("humanized fallback = %q", got)
	}
	if got := translator.Translate("pt", "inbox", "count", 5); got != "5 mensagens" {
		t.Fatalf("plural = %q", got)
	}
	for range 3 {
		translator.Translate("pt-BR", "nowhere.at_all")
	}
	if count := strings.Count(logs.String(), "nowhere.at_all"); count != 1 {
		t.Fatalf("missing key logged %d times:\n%s", count, logs.String())
	}
	if !strings.Contains(logs.String(), "translation missing") || !strings.Contains(logs.String(), "locale=pt-BR") {
		t.Fatalf("log = %s", logs.String())
	}
	// Distinct missing keys from request input cannot grow the log forever.
	logs.Reset()
	for i := range 2000 {
		translator.Translate("en", "flood.k"+strings.Repeat("x", i%50)+string(rune('a'+i%26))+string(rune('a'+i/26%26)))
	}
	if lines := strings.Count(logs.String(), "\n"); lines > 1024 {
		t.Fatalf("logged %d warnings", lines)
	}
}

func TestHTMLEscapesInterpolationOutsideTrustedKeys(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{ShowMissing: true})
	attack := `<script>alert(1)</script>"'&`
	if got := translator.HTML("en", "greeting", "name", attack); strings.Contains(string(got), "<script>") || !strings.Contains(string(got), "&lt;script&gt;") {
		t.Fatalf("plain key in HTML = %q", got)
	}
	got := translator.HTML("en", "users.intro_html", "name", attack)
	if want := `<strong>&lt;script&gt;alert(1)&lt;/script&gt;&#34;&#39;&amp;</strong> joined`; string(got) != want {
		t.Fatalf("_html key = %q, want %q", got, want)
	}
	if got := translator.HTML("en", "users.intro_html", "name", template.HTML("<em>Ana</em>")); string(got) != "<strong><em>Ana</em></strong> joined" {
		t.Fatalf("trusted value = %q", got)
	}
	// A missing _html key is never trusted markup.
	if got := translator.HTML("en", "users.<img src=x onerror=alert(1)>_html"); strings.Contains(string(got), "<img") {
		t.Fatalf("missing _html key = %q", got)
	}
	// Plain-text Translate leaves non-HTML text unescaped for the caller's context.
	if got := translator.Translate("en", "greeting", "name", "<b>"); got != "Hello, <b>!" {
		t.Fatalf("Translate = %q", got)
	}
	// Through html/template, plain translations are escaped like any string.
	var out bytes.Buffer
	page := template.Must(template.New("p").Funcs(template.FuncMap{"t": func(key string, args ...any) string {
		return translator.Translate("en", key, args...)
	}}).Parse(`<p title="{{t "greeting" "name" .}}">{{t "greeting" "name" .}}</p>`))
	if err := page.Execute(&out, attack); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<script>") || strings.Contains(out.String(), `"'&`) {
		t.Fatalf("template output = %s", out.String())
	}
}

func TestIsHTMLKeyAndHumanize(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{"a.b_html": true, "html": true, "a.html": true, "a.htmlx": false, "a_html.b": false, "a.xhtml": false} {
		if i18n.IsHTMLKey(key) != want {
			t.Errorf("IsHTMLKey(%q) != %v", key, want)
		}
	}
	for key, want := range map[string]string{"users.welcome_back": "Welcome back", "a.intro_html": "Intro", "x": "X", "a.é_bom": "É bom", "": ""} {
		if got := i18n.Humanize(key); got != want {
			t.Errorf("Humanize(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestMalformedArgumentsAreReportedNotFatal(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	translator := load(t, i18n.Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if got := translator.Translate("en", "greeting", "name"); got != "Hello, %{name}!" {
		t.Fatalf("odd arguments = %q", got)
	}
	if got := translator.Translate("en", "inbox", "count", "many"); got != "many messages" {
		t.Fatalf("non-numeric count = %q", got)
	}
	if got := translator.Translate("en", "greeting", 3, "x", "name", "Ana"); got != "Hello, Ana!" {
		t.Fatalf("bad name = %q", got)
	}
	if !strings.Contains(logs.String(), "name/value pairs") || !strings.Contains(logs.String(), "not a number") {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestContextHelpers(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	if got := i18n.T(context.Background(), "users.welcome_back"); got != "Welcome back" {
		t.Fatalf("T without translator = %q", got)
	}
	if got := i18n.HTML(context.Background(), "a.<b>"); got != "&lt;b&gt;" {
		t.Fatalf("HTML without translator = %q", got)
	}
	ctx := i18n.NewContext(context.Background(), translator, "pt-BR")
	if i18n.Locale(ctx) != "pt-BR" || i18n.T(ctx, "greeting", "name", "Ana") != "Oi, Ana!" {
		t.Fatalf("T = %q in %q", i18n.T(ctx, "greeting", "name", "Ana"), i18n.Locale(ctx))
	}
	if got := i18n.Locale(i18n.NewContext(context.Background(), translator, "xx")); got != "en" {
		t.Fatalf("unknown locale bound as %q", got)
	}
	if got := i18n.Number(ctx, 1234567.891, -1); got != "1.234.567,89" {
		t.Fatalf("Number = %q", got)
	}
	when := time.Date(2026, time.March, 5, 14, 7, 9, 0, time.UTC)
	if got := i18n.Localize(ctx, when, "date.formats.long"); got != "5 de março de 2026" {
		t.Fatalf("Localize = %q", got)
	}
	if i18n.Number(context.Background(), 12, 0) != "12" || i18n.Localize(context.Background(), when, "") != "2026-03-05T14:07:09Z" {
		t.Fatal("formatting without translator")
	}
}

func TestFormatNumber(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	cases := []struct {
		locale    string
		value     any
		precision int
		want      string
	}{
		{"en", 0, -1, "0"},
		{"en", 999, -1, "999"},
		{"en", 1000, -1, "1,000"},
		{"en", -1234567, -1, "-1,234,567"},
		{"en", uint64(18446744073709551615), -1, "18,446,744,073,709,551,615"},
		{"en", 1234.5, -1, "1,234.50"},
		{"en", 1234.5, 0, "1,234"}, // exact binary ties round half to even
		{"en", 1235.5, 0, "1,236"},
		{"en", 12, 2, "12.00"},
		{"pt-BR", 1234.5, 1, "1.234,5"},
		{"pt-BR", -0.5, -1, "-0,50"},
		{"ja", 1234, -1, "1,234"}, // default locale's format
	}
	for _, test := range cases {
		if got, ok := translator.FormatNumber(test.locale, test.value, test.precision); !ok || got != test.want {
			t.Errorf("FormatNumber(%s, %v, %d) = %q, %v; want %q", test.locale, test.value, test.precision, got, ok, test.want)
		}
	}
	if got, ok := translator.FormatNumber("en", "12", -1); ok || got != "12" {
		t.Errorf("non-number = %q, %v", got, ok)
	}
}

func TestFormatTime(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	when := time.Date(2026, time.March, 5, 14, 7, 9, 0, time.FixedZone("BRT", -3*3600))
	cases := []struct {
		locale, format, want string
	}{
		{"en", "", "Thu, 05 Mar 2026 14:07:09 -0300"},
		{"en", "short", "05 Mar 14:07"},
		{"en", "date.formats.long", "March 5, 2026"},
		{"pt-BR", "date.formats.long", "5 de março de 2026"},
		{"pt-BR", "short", "05/03 14:07"},
		{"pt-BR", "unknown", "Thu, 05 Mar 2026 14:07:09 -0300"}, // falls back to time.formats.default
	}
	for _, test := range cases {
		if got := translator.FormatTime(test.locale, when, test.format); got != test.want {
			t.Errorf("FormatTime(%s, %q) = %q, want %q", test.locale, test.format, got, test.want)
		}
	}
	custom, err := i18n.Load(fstest.MapFS{"en.json": {Data: []byte(`{"en":{"time":{"formats":{"x":"%I:%M %p|%-I%P|%e|%j|%y|%A|%Z|%%|%q|%"}}}}`)}}, i18n.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := custom.FormatTime("en", when, "x"); got != "02:07 PM|2pm| 5|064|26|Thursday|BRT|%|%q|%" {
		t.Errorf("directives = %q", got)
	}
}

func TestParseAcceptLanguage(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"":                        nil,
		"pt-BR,pt;q=0.9,en;q=0.8": {"pt-BR", "pt", "en"},
		"en;q=0.5, fr ; q=1, de":  {"fr", "de", "en"},
		"en;q=0, fr":              {"fr"},
		"*":                       {"*"},
		"en;q=1.5, fr;q=-1, de;q=abc, it;q=0.1234, es;q=0.123": {"es"},
		"e1n, 12, en_US, en-, -en, en--US, , ;q=1, fr":         {"fr"},
		"en-US-x-private;q=0.3, zh-Hant-TW":                    {"zh-Hant-TW", "en-US-x-private"},
		"en;Q=0.2,fr;level=1":                                  {"fr", "en"},
		"en;q=1.000,fr;q=1.0001":                               {"en"},
		"en;q=0., fr;q=1.":                                     {"fr"},
	}
	for header, want := range cases {
		if got := i18n.ParseAcceptLanguage(header); !slices.Equal(got, want) {
			t.Errorf("ParseAcceptLanguage(%q) = %v, want %v", header, got, want)
		}
	}
	if got := i18n.ParseAcceptLanguage("en," + strings.Repeat("x", 4096)); got != nil {
		t.Errorf("an oversized header was parsed: %v", got)
	}
	if got := i18n.ParseAcceptLanguage(strings.Repeat("xx,", 40) + "fr"); got != nil && slices.Contains(got, "fr") {
		t.Errorf("entries beyond the limit were read: %v", got)
	}
}

func TestNegotiate(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	options := i18n.NegotiationOptions{PathPrefix: true, Param: "locale"}
	cases := []struct {
		name, target, cookie, accept, want string
	}{
		{"default", "/", "", "", "en"},
		{"accept exact", "/", "", "pt-BR,en;q=0.5", "pt-BR"},
		{"accept truncation", "/", "", "pt-PT", "pt"},
		{"accept language variant", "/", "", "en-GB", "en"},
		{"accept order by quality", "/", "", "en;q=0.4, pt;q=0.9", "pt"},
		{"accept skips unknown", "/", "", "fr, de;q=0.9, pt-BR;q=0.1", "pt-BR"},
		{"accept wildcard", "/", "", "fr, *;q=0.5", "en"},
		{"accept q=0 excludes", "/", "", "pt;q=0, fr", "en"},
		{"accept malformed", "/", "", ";;;,,,=", "en"},
		{"cookie beats header", "/", "pt", "en", "pt"},
		{"invalid cookie ignored", "/", "../../etc", "pt-BR", "pt-BR"},
		{"param beats cookie", "/?locale=pt-br", "en", "en", "pt-BR"},
		{"unknown param ignored", "/?locale=fr", "", "pt", "pt"},
		{"traversal param ignored", "/?locale=..%2F..%2Fetc", "", "", "en"},
		{"path prefix beats param", "/pt-BR/posts?locale=en", "", "", "pt-BR"},
		{"path prefix must be available", "/fr/posts", "", "pt", "pt"},
	}
	for _, test := range cases {
		request := httptest.NewRequest(http.MethodGet, test.target, nil)
		if test.cookie != "" {
			request.AddCookie(&http.Cookie{Name: "locale", Value: test.cookie})
		}
		if test.accept != "" {
			request.Header.Set("Accept-Language", test.accept)
		}
		if got := translator.Negotiate(request, options); got != test.want {
			t.Errorf("%s: Negotiate = %q, want %q", test.name, got, test.want)
		}
	}
	// Without PathPrefix and Param, the prefix and the parameter are ignored.
	request := httptest.NewRequest(http.MethodGet, "/pt/posts?locale=pt", nil)
	if got := translator.Negotiate(request, i18n.NegotiationOptions{}); got != "en" {
		t.Errorf("opt-in sources used by default: %q", got)
	}
}

func TestMiddleware(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	middleware, err := translator.Middleware(i18n.NegotiationOptions{PathPrefix: true})
	if err != nil {
		t.Fatal(err)
	}
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path + "|" + r.URL.RawPath + "|" + i18n.Locale(r.Context()) + "|" + i18n.T(r.Context(), "greeting", "name", "Ana")))
	}))
	cases := map[string]string{
		"/pt-BR/posts":  "/posts||pt-BR|Oi, Ana!",
		"/pt-BR":        "/||pt-BR|Oi, Ana!",
		"/pt/":          "/||pt|Olá, Ana!",
		"/pt-BRx/posts": "/pt-BRx/posts||en|Hello, Ana!",
		"/posts":        "/posts||en|Hello, Ana!",
		"/pt/a%2Fb":     "/a/b|/a%2Fb|pt|Olá, Ana!",
	}
	for target, want := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Body.String() != want {
			t.Errorf("%s: %q, want %q", target, recorder.Body.String(), want)
		}
		if recorder.Header().Get("Content-Language") != "" || len(recorder.Header().Values("Vary")) != 0 {
			t.Errorf("%s: the middleware set headers %v", target, recorder.Header())
		}
	}
	if _, err := translator.Middleware(i18n.NegotiationOptions{Param: "a b"}); err == nil {
		t.Error("invalid parameter accepted")
	}
	if _, err := translator.Middleware(i18n.NegotiationOptions{Cookie: "a;b"}); err == nil {
		t.Error("invalid cookie name accepted")
	}
}

func TestSetHeaders(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	recorder := httptest.NewRecorder()
	recorder.Header().Add("Vary", "accept-language")
	i18n.SetHeaders(recorder, i18n.NewContext(context.Background(), translator, "pt-BR"))
	i18n.SetHeaders(recorder, i18n.NewContext(context.Background(), translator, "pt-BR"))
	if got := recorder.Header().Get("Content-Language"); got != "pt-BR" {
		t.Fatalf("Content-Language = %q", got)
	}
	if got := recorder.Header().Values("Vary"); !slices.Equal(got, []string{"accept-language", "Cookie"}) {
		t.Fatalf("Vary = %v", got)
	}
	untouched := httptest.NewRecorder()
	i18n.SetHeaders(untouched, context.Background())
	if len(untouched.Header()) != 0 {
		t.Fatalf("headers without a locale: %v", untouched.Header())
	}
}

func TestSetCookie(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	recorder := httptest.NewRecorder()
	if err := translator.SetCookie(recorder, "", "pt-br"); err != nil {
		t.Fatal(err)
	}
	cookie := recorder.Result().Cookies()[0]
	if cookie.Name != "locale" || cookie.Value != "pt-BR" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %+v", cookie)
	}
	for _, locale := range []string{"fr", "../en", ""} {
		if err := translator.SetCookie(httptest.NewRecorder(), "", locale); err == nil {
			t.Errorf("SetCookie(%q) accepted", locale)
		}
	}
	if err := translator.SetCookie(httptest.NewRecorder(), "bad name", "en"); err == nil {
		t.Error("invalid cookie name accepted")
	}
}

func TestCustomPluralRules(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"en.json": {Data: []byte(`{"en":{}}`)},
		"cy.json": {Data: []byte(`{"cy":{"cats":{"two":"dwy gath","other":"%{count} cath"}}}`)},
	}
	rule := func(o i18n.Operands) i18n.Category {
		if o.V == 0 && o.I == 2 {
			return i18n.Two
		}
		return i18n.Other
	}
	translator, err := i18n.Load(files, i18n.Options{PluralRules: map[string]i18n.PluralRule{"CY": rule}})
	if err != nil {
		t.Fatal(err)
	}
	if got := translator.Translate("cy", "cats", "count", 2); got != "dwy gath" {
		t.Fatalf("custom rule = %q", got)
	}
	if got := translator.PluralCategories("cy"); !slices.Equal(got, []i18n.Category{i18n.Two, i18n.Other}) {
		t.Fatalf("PluralCategories = %v", got)
	}
	if _, err := i18n.Load(files, i18n.Options{PluralRules: map[string]i18n.PluralRule{"cy": nil}}); err == nil {
		t.Fatal("nil rule accepted")
	}
	if _, err := i18n.Load(files, i18n.Options{PluralRules: map[string]i18n.PluralRule{"c/y": rule}}); err == nil {
		t.Fatal("invalid rule locale accepted")
	}
}

func TestFileLocale(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"en.json": "en", "posts.pt-BR.json": "pt-BR", "admin.users.zh-Hant-TW.json": "zh-Hant-TW"} {
		if got, err := i18n.FileLocale(name); err != nil || got != want {
			t.Errorf("FileLocale(%q) = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"en.yml", "../en.json", "a/en.json", `a\en.json`, "pt-br.json", "Posts.en.json", ".json", "x..en.json"} {
		if got, err := i18n.FileLocale(name); err == nil {
			t.Errorf("FileLocale(%q) = %q, want an error", name, got)
		}
	}
}
