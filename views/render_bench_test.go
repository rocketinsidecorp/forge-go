package views_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/views"
	"github.com/rocketinsidecorp/forge-go/web"
)

var benchSite = fstest.MapFS{
	"layouts/application.gohtml": {Data: []byte(`<!doctype html><html lang="en"><head><title>{{block "title" .}}App{{end}}</title>{{csrfMeta}}</head>` +
		`<body>{{template "shared/_flash" .}}<main>{{block "content" .}}{{end}}</main></body></html>`)},
	"shared/_flash.gohtml": {Data: []byte(`{{range flashes}}<p class="flash flash-{{.Kind}}">{{.Text}}</p>{{end}}`)},
	"posts/_row.gohtml":    {Data: []byte(`<tr id="{{domID "post" .ID}}"><td><a href="/posts/{{.ID}}">{{.Title}}</a></td><td>{{.Body}}</td></tr>`)},
	"posts/_form.gohtml":   {Data: []byte(`<form method="post" action="/posts">{{csrfField}}<input name="title"></form>`)},
	"posts/index.gohtml": {Data: []byte(`{{define "title"}}Posts{{end}}{{define "content"}}<h1>Posts</h1>` +
		`<table>{{range .}}{{template "posts/_row" .}}{{end}}</table>{{template "posts/_form" .}}{{end}}`)},
}

// benchPlainSite is benchSite without the helpers that need a request.
var benchPlainSite = fstest.MapFS{
	"layouts/application.gohtml": {Data: []byte(`<!doctype html><html lang="en"><head><title>{{block "title" .}}App{{end}}</title></head>` +
		`<body><main>{{block "content" .}}{{end}}</main></body></html>`)},
	"posts/_row.gohtml":  benchSite["posts/_row.gohtml"],
	"posts/index.gohtml": {Data: []byte(`{{define "title"}}Posts{{end}}{{define "content"}}<h1>Posts</h1><table>{{range .}}{{template "posts/_row" .}}{{end}}</table>{{end}}`)},
}

// BenchmarkHTMLRender is scenario "html_render" in docs/BENCHMARKS.md: a page
// of 50 rows rendered into a layout with two partials, with HTML
// auto-escaping of the row text (which contains markup) and a discarding
// writer. "layout_partial" has no request helpers; "layout_partial_csrf" is
// the page a generated app serves: it also embeds the CSRF meta tags and a
// form token and flash messages, through the csrf and flash middleware.
func BenchmarkHTMLRender(b *testing.B) {
	type row struct {
		ID          uuid.UUID
		Title, Body string
	}
	rows := make([]row, 50)
	for i := range rows {
		rows[i] = row{ID: uuid.MustNew(), Title: fmt.Sprintf("Post <%d> & friends", i), Body: `Tom & Jerry say "hello" <b>world</b>`}
	}
	b.Run("layout_partial", func(b *testing.B) {
		renderer, err := views.Load(benchPlainSite, views.Options{})
		if err != nil {
			b.Fatal(err)
		}
		w := benchWriter{header: http.Header{}}
		b.ReportAllocs()
		for b.Loop() {
			if err := renderer.Render(w, "posts/index", http.StatusOK, rows); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("layout_partial_csrf", func(b *testing.B) {
		renderer, err := views.Load(benchSite, views.Options{})
		if err != nil {
			b.Fatal(err)
		}
		protection, err := csrf.New(csrf.Options{})
		if err != nil {
			b.Fatal(err)
		}
		handler := web.Flash(protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := renderer.RenderRequest(w, r, "posts/index", http.StatusOK, rows); err != nil {
				b.Error(err)
			}
		})))
		token, err := csrf.NewToken()
		if err != nil {
			b.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/posts", nil)
		request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
		w := benchWriter{header: http.Header{}}
		b.ReportAllocs()
		for b.Loop() {
			handler.ServeHTTP(w, request)
		}
	})
}

type benchWriter struct{ header http.Header }

func (w benchWriter) Header() http.Header          { return w.header }
func (benchWriter) Write(data []byte) (int, error) { return len(data), nil }
func (benchWriter) WriteHeader(int)                {}
