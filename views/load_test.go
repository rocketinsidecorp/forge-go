package views_test

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/views"
	"github.com/rocketinsidecorp/forge-go/web"
)

var site = fstest.MapFS{
	"layouts/application.gohtml": {Data: []byte(`<!doctype html><html lang="en"><head><title>{{block "title" .}}App{{end}}</title>{{csrfMeta}}</head>` +
		`<body>{{template "shared/_flash" .}}<main>{{block "content" .}}{{end}}</main></body></html>`)},
	"layouts/plain.gohtml":  {Data: []byte(`<plain>{{block "content" .}}{{end}}</plain>`)},
	"shared/_flash.gohtml":  {Data: []byte(`{{range flashes}}<p class="flash flash-{{.Kind}}">{{.Text}}</p>{{end}}`)},
	"posts/_form.gohtml":    {Data: []byte(`<form method="post" action="{{.Action}}">{{csrfField}}{{with .Method}}{{methodField .}}{{end}}<input name="title" value="{{.Post.Title}}"></form>`)},
	"posts/index.gohtml":    {Data: []byte(`{{define "title"}}Posts{{end}}{{define "content"}}<h1>Posts</h1>{{range .}}<p id="{{domID "post" .ID}}"><a href="/posts/{{.ID}}">{{.Title}}</a></p>{{end}}{{end}}`)},
	"posts/new.gohtml":      {Data: []byte(`{{define "title"}}New post{{end}}{{define "content"}}<h1>New</h1>{{template "posts/_form" (dict "Action" "/posts" "Method" "" "Post" .)}}{{end}}`)},
	"posts/edit.gohtml":     {Data: []byte(`{{define "content"}}<h1>Edit</h1>{{template "posts/_form" (dict "Action" (printf "/posts/%s" .ID) "Method" "patch" "Post" .)}}{{end}}`)},
	"admin/posts/show.html": {Data: []byte(`ignored: not .gohtml`)},
	"standalone.gohtml":     {Data: []byte(`<!doctype html><p>{{.}}</p>`)},
	".hidden/page.gohtml":   {Data: []byte(`{{.Broken`)},
}

type post struct {
	ID    uuid.UUID
	Title string
}

func load(t *testing.T, files fstest.MapFS, opts views.Options) *views.Renderer {
	t.Helper()
	renderer, err := views.Load(files, opts)
	if err != nil {
		t.Fatal(err)
	}
	return renderer
}

// serve runs a render inside the CSRF and flash middleware, as an
// application does.
func serve(t *testing.T, renderer *views.Renderer, name string, status int, data any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	handler := web.Flash(protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := renderer.RenderRequest(w, r, name, status, data); err != nil {
			t.Errorf("render %s: %v", name, err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})))
	request := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestLoadRendersPagesInsideTheLayout(t *testing.T) {
	t.Parallel()
	renderer := load(t, site, views.Options{})
	if got := strings.Join(renderer.Pages(), ","); got != "posts/edit,posts/index,posts/new,standalone" {
		t.Fatalf("pages = %s", got)
	}
	first := post{ID: uuid.MustNew(), Title: `<script>alert("x")</script>`}
	recorder := serve(t, renderer, "posts/index", http.StatusOK, []post{first, {Title: "draft"}})
	body := recorder.Body.String()
	for _, want := range []string{
		"<title>Posts</title>",
		"<h1>Posts</h1>",
		`id="post_` + first.ID.String() + `"`,
		`id="new_post"`,
		`href="/posts/` + first.ID.String() + `"`,
		"&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;",
		`<meta name="csrf-param" content="authenticity_token">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatalf("unescaped data: %s", body)
	}
	if recorder.Header().Get("Content-Security-Policy") != views.DefaultContentSecurityPolicy || recorder.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("headers = %v", recorder.Header())
	}
	cookie := recorder.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != csrf.CookieName || !strings.Contains(body, `<meta name="csrf-token" content="`+cookie[0].Value+`">`) {
		t.Fatalf("token meta does not match the issued cookie: %v\n%s", cookie, body)
	}
}

func TestLoadKeepsEachPagesBlocksSeparate(t *testing.T) {
	t.Parallel()
	renderer := load(t, site, views.Options{})
	token, err := csrf.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: csrf.CookieName, Value: token}
	existing := post{ID: uuid.MustNew(), Title: `"quoted" & <b>`}

	created := serve(t, renderer, "posts/new", http.StatusUnprocessableEntity, existing, cookie)
	edited := serve(t, renderer, "posts/edit", http.StatusOK, existing, cookie)
	index := serve(t, renderer, "posts/index", http.StatusOK, nil, cookie)

	if created.Code != http.StatusUnprocessableEntity || !strings.Contains(created.Body.String(), "<title>New post</title>") ||
		!strings.Contains(created.Body.String(), `action="/posts"`) || strings.Contains(created.Body.String(), "_method") {
		t.Fatalf("new: %d %s", created.Code, created.Body.String())
	}
	editBody := edited.Body.String()
	for _, want := range []string{
		"<title>App</title>", "<h1>Edit</h1>",
		`action="/posts/` + existing.ID.String() + `"`,
		`<input type="hidden" name="_method" value="PATCH">`,
		`<input type="hidden" name="authenticity_token" value="` + token + `">`,
		`value="&#34;quoted&#34; &amp; &lt;b&gt;"`,
	} {
		if !strings.Contains(editBody, want) {
			t.Fatalf("edit: missing %q in %s", want, editBody)
		}
	}
	if !strings.Contains(index.Body.String(), "<title>Posts</title>") || strings.Contains(index.Body.String(), "Edit") {
		t.Fatalf("index: %s", index.Body.String())
	}
	if created.Header().Get("Set-Cookie") != "" || edited.Header().Get("Set-Cookie") != "" {
		t.Fatal("an existing valid token was replaced")
	}
}

func TestLoadRendersFlashMessagesEscaped(t *testing.T) {
	t.Parallel()
	renderer := load(t, site, views.Options{})
	var cookie *http.Cookie
	recorder := httptest.NewRecorder()
	web.Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = web.SetFlash(w, r, web.FlashNotice, `Saved <img src=x onerror=alert(1)>`)
	})).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	for _, current := range recorder.Result().Cookies() {
		if current.Name == web.FlashCookieName {
			cookie = current
		}
	}
	body := serve(t, renderer, "posts/index", http.StatusOK, nil, cookie).Body.String()
	if !strings.Contains(body, `<p class="flash flash-notice">Saved &lt;img src=x onerror=alert(1)&gt;</p>`) {
		t.Fatalf("flash not rendered escaped: %s", body)
	}
}

func TestLoadRendersStandaloneAndAlternativeLayoutPages(t *testing.T) {
	t.Parallel()
	renderer := load(t, site, views.Options{Layout: "plain"})
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "standalone", 0, "<b>"); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != "<!doctype html><p>&lt;b&gt;</p>" || recorder.Code != http.StatusOK {
		t.Fatalf("standalone: %d %q", recorder.Code, recorder.Body.String())
	}
	plain := httptest.NewRecorder()
	if err := renderer.Render(plain, "posts/index", http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	if plain.Body.String() != "<plain><h1>Posts</h1></plain>" {
		t.Fatalf("plain layout: %q", plain.Body.String())
	}
}

func TestRequestHelpersFailClosedWithoutRequest(t *testing.T) {
	t.Parallel()
	renderer := load(t, site, views.Options{})
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "posts/new", http.StatusOK, post{}); err == nil {
		t.Fatal("rendered a CSRF form without a request")
	}
	if recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
		t.Fatalf("partial response: %v %q", recorder.Header(), recorder.Body.String())
	}
	// With a request but without the middleware, the token is unavailable.
	withoutMiddleware := httptest.NewRecorder()
	err := renderer.RenderRequest(withoutMiddleware, httptest.NewRequest(http.MethodGet, "/", nil), "posts/new", http.StatusOK, post{})
	if !errors.Is(err, csrf.ErrNotInstalled) || withoutMiddleware.Body.Len() != 0 {
		t.Fatalf("error = %v body=%q", err, withoutMiddleware.Body.String())
	}
	if err := renderer.Render(httptest.NewRecorder(), "posts/missing", http.StatusOK, nil); err == nil {
		t.Fatal("rendered an unknown page")
	}
	if err := renderer.Render(httptest.NewRecorder(), "posts/_form", http.StatusOK, nil); err == nil {
		t.Fatal("rendered a partial as a page")
	}
	if err := renderer.Render(httptest.NewRecorder(), "standalone", 1000, nil); err == nil {
		t.Fatal("accepted an invalid status")
	}
}

func TestLoadRejectsInvalidTrees(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files fstest.MapFS
		opts  views.Options
	}{
		{name: "empty", files: fstest.MapFS{}},
		{name: "missing layout", files: fstest.MapFS{"posts/index.gohtml": {Data: []byte(`{{define "content"}}x{{end}}`)}}},
		{name: "unknown layout", files: site, opts: views.Options{Layout: "admin"}},
		{name: "invalid layout name", files: site, opts: views.Options{Layout: "../x"}},
		{name: "space in path", files: fstest.MapFS{"my page.gohtml": {Data: []byte(`x`)}}},
		{name: "parse error", files: fstest.MapFS{"page.gohtml": {Data: []byte(`{{.Broken`)}}},
		{name: "unknown function", files: fstest.MapFS{"page.gohtml": {Data: []byte(`{{shout .}}`)}}},
		{name: "unknown partial", files: fstest.MapFS{"page.gohtml": {Data: []byte(`{{template "shared/_missing" .}}`)}}},
		{name: "escaping error", files: fstest.MapFS{"page.gohtml": {Data: []byte(`<a href="{{.}}`)}}},
		{name: "built-in function replaced", files: site, opts: views.Options{Funcs: template.FuncMap{"csrfField": func() string { return "" }}}},
		{name: "nil function", files: site, opts: views.Options{Funcs: template.FuncMap{"shout": nil}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := views.Load(tt.files, tt.opts); err == nil {
				t.Fatal("Load succeeded")
			}
		})
	}
	if _, err := views.Load(nil, views.Options{}); err == nil {
		t.Fatal("Load(nil) succeeded")
	}
}

func TestLoadAcceptsApplicationFunctionsAndPolicy(t *testing.T) {
	t.Parallel()
	renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`{{shout .}}`)}}, views.Options{
		Funcs:                 template.FuncMap{"shout": strings.ToUpper},
		ContentSecurityPolicy: "default-src 'self'",
	})
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "page", http.StatusOK, "<hi>"); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != "&lt;HI&gt;" || recorder.Header().Get("Content-Security-Policy") != "default-src 'self'" {
		t.Fatalf("%v %q", recorder.Header(), recorder.Body.String())
	}
}

func TestHelpersRejectUnsafeArguments(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`{{methodField "GET"}}`,
		`{{methodField "<x>"}}`,
		`{{domID "Post" 1}}`,
		`{{domID "post" "a b"}}`,
		`{{domID "post" "\"><script>"}}`,
		`{{domID "post" 1.5}}`,
		`{{dict "a"}}`,
		`{{dict 1 2}}`,
	} {
		renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(source)}}, views.Options{})
		recorder := httptest.NewRecorder()
		if err := renderer.Render(recorder, "page", http.StatusOK, nil); err == nil || recorder.Body.Len() != 0 {
			t.Fatalf("%s: err=%v body=%q", source, err, recorder.Body.String())
		}
	}
	renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`{{domID "edit_post" 7}} {{domID "post" "a-1"}} {{methodField "delete"}}`)}}, views.Options{})
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "page", http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != `edit_post_7 post_a-1 <input type="hidden" name="_method" value="DELETE">` {
		t.Fatalf("%q", recorder.Body.String())
	}
}

// Pooled template clones carry per-request state; concurrent renders must
// never see each other's request (run with -race).
func TestConcurrentRendersKeepTheirOwnRequest(t *testing.T) {
	t.Parallel()
	renderer := load(t, site, views.Options{})
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	handler := protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := renderer.RenderRequest(w, r, "posts/new", http.StatusOK, post{Title: r.URL.Query().Get("title")}); err != nil {
			t.Error(err)
		}
	}))
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			token, err := csrf.NewToken()
			if err != nil {
				t.Error(err)
				return
			}
			request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("https://app.example.com/?title=t%d", i), nil)
			request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if !strings.Contains(body, `value="`+token+`"`) || !strings.Contains(body, fmt.Sprintf(`value="t%d"`, i)) {
				t.Errorf("render %d mixed requests: %s", i, body)
			}
		})
	}
	wg.Wait()
}
