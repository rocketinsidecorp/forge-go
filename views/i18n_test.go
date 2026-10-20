package views_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/i18n"
	"github.com/rocketinsidecorp/forge-go/views"
)

var translations = fstest.MapFS{
	"en.json": {Data: []byte(`{"en": {
		"posts": {"title": "Posts", "greeting": "Hello, %{name}", "intro_html": "<em>%{name}</em> wrote", "count": {"one": "%{count} post", "other": "%{count} posts"}},
		"time": {"formats": {"short": "%b %-d"}}
	}}`)},
	"pt-BR.json": {Data: []byte(`{"pt-BR": {
		"posts": {"title": "Publicações", "count": {"one": "%{count} publicação", "other": "%{count} publicações"}},
		"number": {"format": {"separator": ",", "delimiter": "."}},
		"date": {"abbr_month_names": [null, "jan", "fev", "mar", "abr", "mai", "jun", "jul", "ago", "set", "out", "nov", "dez"]},
		"time": {"formats": {"short": "%-d %b"}}
	}}`)},
}

var translatedSite = fstest.MapFS{
	"layouts/application.gohtml": {Data: []byte(`<html lang="{{locale}}"><title>{{block "title" .}}{{t "posts.title"}}{{end}}</title>{{block "content" .}}{{end}}</html>`)},
	"posts/index.gohtml": {Data: []byte(`{{define "content"}}<h1>{{t "posts.title"}}</h1>` +
		`<p title="{{t "posts.greeting" "name" .Name}}">{{t "posts.greeting" "name" .Name}}</p>` +
		`<p>{{t "posts.intro_html" "name" .Name}}</p>` +
		`<p>{{t "posts.count" "count" .Count}}</p><p>{{number .Total}}</p><p>{{l .When "short"}}</p>{{end}}`)},
	"posts/_badge.gohtml": {Data: []byte(`<span>{{t "posts.title"}}</span>`)},
}

type translatedPage struct {
	Name  string
	Count int
	Total float64
	When  time.Time
}

func translator(t *testing.T) *i18n.Translator {
	t.Helper()
	loaded, err := i18n.Load(translations, i18n.Options{ShowMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestTranslationHelpersUseTheRequestLocale(t *testing.T) {
	t.Parallel()
	translated := translator(t)
	renderer := load(t, translatedSite, views.Options{})
	middleware, err := translated.Middleware(i18n.NegotiationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	attack := `"><script>alert(1)</script>`
	data := translatedPage{Name: attack, Count: 2, Total: 1234.5, When: time.Date(2026, time.February, 3, 0, 0, 0, 0, time.UTC)}
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := renderer.RenderRequest(w, r, "posts/index", http.StatusOK, data); err != nil {
			t.Error(err)
		}
	}))
	render := func(language string) string {
		request := httptest.NewRequest(http.MethodGet, "/posts", nil)
		request.Header.Set("Accept-Language", language)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Body.String()
	}

	english := render("en-US")
	for _, want := range []string{
		`<html lang="en">`, "<title>Posts</title>", "<h1>Posts</h1>", "<p>2 posts</p>", "<p>1,234.50</p>", "<p>Feb 3</p>",
		`title="Hello, &#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`,
		`">Hello, &#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;</p>`,
		`<p><em>&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;</em> wrote</p>`,
	} {
		if !strings.Contains(english, want) {
			t.Errorf("English page lacks %s:\n%s", want, english)
		}
	}
	if strings.Contains(english, "<script>") {
		t.Fatalf("an argument escaped its context:\n%s", english)
	}

	portuguese := render("pt-BR,pt;q=0.9")
	for _, want := range []string{
		`<html lang="pt-BR">`, "<h1>Publicações</h1>", "<p>2 publicações</p>", "<p>1.234,50</p>", "<p>3 fev</p>",
		"translation missing: pt-BR.posts.greeting",
	} {
		if !strings.Contains(portuguese, want) {
			t.Errorf("Portuguese page lacks %s:\n%s", want, portuguese)
		}
	}
}

func TestTranslationHelpersNeedATranslator(t *testing.T) {
	t.Parallel()
	// Load succeeds: the helpers are resolved per request.
	renderer := load(t, translatedSite, views.Options{})
	recorder := httptest.NewRecorder()
	err := renderer.RenderRequest(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "posts/index", http.StatusOK, translatedPage{})
	if err == nil || !strings.Contains(err.Error(), "i18n.Translator.Middleware") {
		t.Fatalf("render without a translator: %v", err)
	}
	if _, err := renderer.Fragment(nil, "posts/_badge", nil); err == nil {
		t.Fatal("a fragment without request or Options.I18n rendered")
	}
	// Options.I18n serves request-less fragments in the default locale.
	withDefault := load(t, translatedSite, views.Options{I18n: translator(t)})
	badge, err := withDefault.Fragment(nil, "posts/_badge", nil)
	if err != nil || string(badge) != "<span>Posts</span>" {
		t.Fatalf("fragment = %q, %v", badge, err)
	}
}

func TestTranslationHelperArgumentsAreCheckedAtLoad(t *testing.T) {
	t.Parallel()
	for name, page := range map[string]string{
		"odd arguments":   `<p>{{t "posts.greeting" "name"}}</p>`,
		"non-string name": `<p>{{t "posts.greeting" 1 "x"}}</p>`,
	} {
		_, err := views.Load(fstest.MapFS{"posts/x.gohtml": {Data: []byte(page)}}, views.Options{})
		if err == nil || !strings.Contains(err.Error(), "t \"posts.greeting\"") {
			t.Errorf("%s: Load error = %v", name, err)
		}
	}
}
