package forge_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/i18n"
)

// denyAdmin is a path guard of the kind forge-go#56 is about: it refuses
// /admin and everything under it by comparing the request path.
func denyAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A locale path prefix (i18n.NegotiationOptions.PathPrefix) is removed
// before the first application middleware runs, so a path guard sees the
// path the router serves wherever it is registered: with WithHTTPMiddleware
// or App.Use, before or after the i18n middleware (forge-go#56).
func TestALocalePrefixCannotBypassAPathGuardRegisteredBeforeTheNegotiation(t *testing.T) {
	t.Parallel()
	translator, err := i18n.Load(fstest.MapFS{
		"en.json":    {Data: []byte(`{"en": {"hello": "Hello"}}`)},
		"pt-BR.json": {Data: []byte(`{"pt-BR": {"hello": "Olá"}}`)},
	}, i18n.Options{})
	if err != nil {
		t.Fatal(err)
	}
	negotiation, err := translator.Middleware(i18n.NegotiationOptions{PathPrefix: true})
	if err != nil {
		t.Fatal(err)
	}
	// Each order registers the guard and the negotiation with
	// WithHTTPMiddleware ("outer") or App.Use ("use"), first to last.
	type step struct {
		outer      bool
		middleware func(http.Handler) http.Handler
	}
	guardOuter, guardUse := step{true, denyAdmin}, step{false, denyAdmin}
	i18nOuter, i18nUse := step{true, negotiation}, step{false, negotiation}
	orders := map[string][]step{
		"guard outer, i18n use":        {guardOuter, i18nUse},
		"guard use, then i18n use":     {guardUse, i18nUse},
		"i18n use, then guard use":     {i18nUse, guardUse},
		"i18n outer, guard use":        {i18nOuter, guardUse},
		"guard outer, then i18n outer": {guardOuter, i18nOuter},
		"i18n outer, then guard outer": {i18nOuter, guardOuter},
		// Two negotiation layers still remove one prefix.
		"guard use between two i18n": {i18nOuter, guardUse, i18nUse},
	}
	cases := []struct {
		target string
		status int
		body   string // "" when only the status matters
	}{
		{"/admin", http.StatusForbidden, ""},
		{"/pt-BR/admin", http.StatusForbidden, ""},
		{"/PT-br/admin/users", http.StatusForbidden, ""},
		{"/en/admin/", http.StatusForbidden, ""},
		{"/pt-BR/admin?x=1", http.StatusForbidden, ""},
		// Percent-encoded spellings of the prefix or the guarded segment.
		{"/pt-BR/%61dmin", http.StatusForbidden, ""},
		{"/%70t-BR/admin", http.StatusForbidden, ""},
		{"/pt-BR%2Fadmin", http.StatusForbidden, ""},
		{"/pt-BR/admin%2Fusers", http.StatusForbidden, ""},
		// A dot segment left after the prefix is refused by the router.
		{"/pt-BR%2F..%2Fadmin", http.StatusBadRequest, ""},
		// The prefix is removed once: /en after it is a path, not a locale.
		{"/pt-BR/en/admin", http.StatusNotFound, ""},
		{"/pt-BR/pt-BR/admin", http.StatusNotFound, ""},
		{"/en/pt-BR/admin/users", http.StatusNotFound, ""},
		// Unguarded pages keep the prefix behavior.
		{"/pt-BR/posts", http.StatusOK, "/posts|pt-BR|/pt-BR"},
		{"/PT-br/posts", http.StatusOK, "/posts|pt-BR|/pt-BR"},
		{"/posts", http.StatusOK, "/posts|en|"},
	}
	for name, order := range orders {
		var options []forge.Option
		for _, current := range order {
			if current.outer {
				options = append(options, forge.WithHTTPMiddleware(current.middleware))
			}
		}
		app, err := forge.New(options...)
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range order {
			if !current.outer {
				if err := app.Use(current.middleware); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, pattern := range []string{"GET /admin", "GET /admin/{rest...}", "GET /posts"} {
			if err := app.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(r.URL.Path + "|" + i18n.Locale(r.Context()) + "|" + i18n.PathPrefix(r.Context())))
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range cases {
			recorder := httptest.NewRecorder()
			app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.target, nil))
			if recorder.Code != test.status || (test.body != "" && recorder.Body.String() != test.body) {
				t.Errorf("%s, %s: status=%d body=%q, want %d %q", name, test.target, recorder.Code, recorder.Body.String(), test.status, test.body)
			}
		}
	}
}

// The middleware added with App.Use is frozen before the first request
// reaches any application middleware, so the rewrites the server applies
// always belong to the chain that serves the request.
func TestUseIsFrozenOnceAWithHTTPMiddlewareRejectsTheFirstRequest(t *testing.T) {
	t.Parallel()
	app, err := forge.New(forge.WithHTTPMiddleware(denyAdmin))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d", recorder.Code)
	}
	if err := app.Use(denyAdmin); err == nil {
		t.Fatal("Use after the first request was accepted")
	}
}
