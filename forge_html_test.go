package forge_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/router"
	"github.com/rocketinsidecorp/forge-go/views"
	"github.com/rocketinsidecorp/forge-go/web"
)

func TestUseOrdersMiddlewareAndFreezesAfterFirstRequest(t *testing.T) {
	t.Parallel()
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	mark := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("X-Order", name)
				next.ServeHTTP(w, r)
			})
		}
	}
	for _, name := range []string{"first", "second"} {
		if err := app.Use(mark(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Use(nil); err == nil {
		t.Fatal("nil middleware accepted")
	}
	if err := app.Use(func(http.Handler) http.Handler { return nil }); err == nil {
		t.Fatal("middleware returning nil accepted")
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if got := strings.Join(recorder.Header().Values("X-Order"), ","); got != "first,second" || recorder.Code != http.StatusOK {
		t.Fatalf("order=%q status=%d", got, recorder.Code)
	}
	if err := app.Use(mark("late")); !errors.Is(err, router.ErrFrozen) {
		t.Fatalf("Use after serving: %v", err)
	}
}

// postsHandler is a minimal HTML resource built only from framework APIs, the
// shape `forge generate scaffold` output takes.
type postsHandler struct {
	views  *views.Renderer
	titles map[string]string
}

type postForm struct {
	Title string `form:"title"`
}

func (h *postsHandler) Index(w http.ResponseWriter, r *http.Request) {
	if err := h.views.RenderRequest(w, r, "posts/index", http.StatusOK, h.titles); err != nil {
		web.Error(w, r, err)
	}
}

func (h *postsHandler) New(w http.ResponseWriter, r *http.Request) {
	if err := h.views.RenderRequest(w, r, "posts/new", http.StatusOK, map[string]any{"Form": postForm{}}); err != nil {
		web.Error(w, r, err)
	}
}

func (h *postsHandler) Create(w http.ResponseWriter, r *http.Request) {
	var form postForm
	if err := web.DecodeForm(r, &form); err != nil {
		web.Error(w, r, err)
		return
	}
	if strings.TrimSpace(form.Title) == "" {
		problems := web.FormErrors{{Field: "title", Code: "blank", Message: "can't be blank"}}
		if err := h.views.RenderRequest(w, r, "posts/new", http.StatusUnprocessableEntity, map[string]any{"Form": form, "Errors": problems}); err != nil {
			web.Error(w, r, err)
		}
		return
	}
	h.titles["1"] = form.Title
	if err := web.SetFlash(w, r, web.FlashNotice, "Post was created."); err != nil {
		web.Error(w, r, err)
		return
	}
	if err := web.Redirect(w, "/posts"); err != nil {
		web.Error(w, r, err)
	}
}

func (h *postsHandler) Destroy(w http.ResponseWriter, r *http.Request) {
	delete(h.titles, r.PathValue("id"))
	if err := web.SetFlash(w, r, web.FlashNotice, "Post was deleted."); err != nil {
		web.Error(w, r, err)
		return
	}
	if err := web.Redirect(w, "/posts"); err != nil {
		web.Error(w, r, err)
	}
}

var postViews = fstest.MapFS{
	"layouts/application.gohtml": {Data: []byte(`<!doctype html><title>{{block "title" .}}App{{end}}</title>{{csrfMeta}}{{template "shared/_flash" .}}<main>{{block "content" .}}{{end}}</main>`)},
	"shared/_flash.gohtml":       {Data: []byte(`{{range flashes}}<p class="flash-{{.Kind}}">{{.Text}}</p>{{end}}`)},
	"posts/index.gohtml":         {Data: []byte(`{{define "content"}}{{range $id, $title := .}}<form method="post" action="/posts/{{$id}}">{{methodField "delete"}}{{csrfField}}<button>Delete {{$title}}</button></form>{{end}}{{end}}`)},
	"posts/new.gohtml": {Data: []byte(`{{define "content"}}<form method="post" action="/posts">{{csrfField}}<input name="title" value="{{.Form.Title}}">` +
		`{{with .Errors}}<p class="error">{{.Message "title"}}</p>{{end}}</form>{{end}}`)},
}

var tokenField = regexp.MustCompile(`name="authenticity_token" value="([A-Za-z0-9_-]+)"`)

// TestHTMLRequestPipeline drives the server-rendered request cycle through
// forge.App: CSRF, method override, flash, form decoding, 422 re-rendering
// and redirects.
func TestHTMLRequestPipeline(t *testing.T) {
	t.Parallel()
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, middleware := range []func(http.Handler) http.Handler{web.MethodOverride, protection.Handler, web.Flash} {
		if err := app.Use(middleware); err != nil {
			t.Fatal(err)
		}
	}
	renderer, err := views.Load(postViews, views.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Resources("/posts", &postsHandler{views: renderer, titles: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	cookies := map[string]*http.Cookie{}
	send := func(method, target, body string, header http.Header) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "https://app.example.com"+target, strings.NewReader(body))
		if body != "" {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		request.Header.Set("Accept", "text/html")
		for name, values := range header {
			request.Header[name] = values
		}
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		for _, cookie := range recorder.Result().Cookies() {
			if cookie.MaxAge < 0 {
				delete(cookies, cookie.Name)
			} else {
				cookies[cookie.Name] = cookie
			}
		}
		return recorder
	}

	page := send(http.MethodGet, "/posts/new", "", nil)
	match := tokenField.FindStringSubmatch(page.Body.String())
	// The form embeds the cookie's token masked (csrf.Token), never as is.
	if page.Code != http.StatusOK || match == nil || cookies[csrf.CookieName] == nil ||
		cookies[csrf.CookieName].Value == match[1] || !csrf.Match(cookies[csrf.CookieName].Value, match[1]) {
		t.Fatalf("new: %d %v %s", page.Code, cookies, page.Body.String())
	}
	token := match[1]

	if forged := send(http.MethodPost, "/posts", "title=forged", nil); forged.Code != http.StatusForbidden {
		t.Fatalf("POST without token: %d", forged.Code)
	}
	crossSite := send(http.MethodPost, "/posts", url.Values{"title": {"x"}, "authenticity_token": {token}}.Encode(), http.Header{"Origin": {"https://evil.example"}})
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", crossSite.Code)
	}

	invalid := send(http.MethodPost, "/posts", url.Values{"title": {" "}, "authenticity_token": {token}}.Encode(), nil)
	if invalid.Code != http.StatusUnprocessableEntity || !strings.Contains(invalid.Body.String(), `<p class="error">can&#39;t be blank</p>`) {
		t.Fatalf("invalid POST: %d %s", invalid.Code, invalid.Body.String())
	}

	created := send(http.MethodPost, "/posts", url.Values{"title": {"<Hello>"}, "authenticity_token": {token}}.Encode(), nil)
	if created.Code != http.StatusSeeOther || created.Header().Get("Location") != "/posts" || cookies[web.FlashCookieName] == nil {
		t.Fatalf("create: %d %v", created.Code, created.Header())
	}
	index := send(http.MethodGet, "/posts", "", nil)
	body := index.Body.String()
	if !strings.Contains(body, `<p class="flash-notice">Post was created.</p>`) || !strings.Contains(body, "Delete &lt;Hello&gt;") || cookies[web.FlashCookieName] != nil {
		t.Fatalf("index after create: %s (cookies %v)", body, cookies)
	}
	if again := send(http.MethodGet, "/posts", "", nil); strings.Contains(again.Body.String(), "flash-notice") {
		t.Fatal("flash shown twice")
	}

	if forgedDelete := send(http.MethodPost, "/posts/1", "_method=DELETE", nil); forgedDelete.Code != http.StatusForbidden {
		t.Fatalf("overridden DELETE without token: %d", forgedDelete.Code)
	}
	deleted := send(http.MethodPost, "/posts/1", url.Values{"_method": {"DELETE"}, "authenticity_token": {token}}.Encode(), nil)
	if deleted.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body.String())
	}
	if after := send(http.MethodGet, "/posts", "", nil); strings.Contains(after.Body.String(), "Hello") || !strings.Contains(after.Body.String(), "Post was deleted.") {
		t.Fatalf("index after delete: %s", after.Body.String())
	}
}
