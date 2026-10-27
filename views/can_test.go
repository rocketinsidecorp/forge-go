package views_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authhttp"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/views"
)

// The can helper shows what the signed-in principal may use and hides the
// rest; without a principal (no Forge identity) it hides nothing.
func TestCanFollowsThePrincipalsPermissions(t *testing.T) {
	t.Parallel()
	renderer := load(t, fstest.MapFS{
		"page.gohtml":  {Data: []byte(`{{if can "posts:write"}}[write]{{end}}{{if can "posts:read"}}[read]{{end}}`)},
		"_frag.gohtml": {Data: []byte(`{{if can "posts:read"}}[read]{{end}}`)},
	}, views.Options{})
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	read, err := auth.NewPermission("posts:read")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant, read)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := authhttp.NewMiddleware(auth.ResolverFunc(func(context.Context, auth.Digest) (auth.Session, bool, error) {
		return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, true, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := renderer.RenderRequest(w, r, "page", http.StatusOK, nil); err != nil {
			t.Error(err)
		}
	})
	for name, test := range map[string]struct {
		handler http.Handler
		header  string
		want    string
	}{
		"a principal with posts:read": {middleware.Authenticate(page), "Bearer " + token.Reveal(), "[read]"},
		"no principal":                {page, "", "[write][read]"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		if test.header != "" {
			request.Header.Set("Authorization", test.header)
		}
		recorder := httptest.NewRecorder()
		test.handler.ServeHTTP(recorder, request)
		if body := strings.TrimSpace(recorder.Body.String()); body != test.want {
			t.Errorf("%s: %q, want %q", name, body, test.want)
		}
	}
	if fragment, err := renderer.Fragment(nil, "_frag", nil); err != nil || string(fragment) != "[read]" {
		t.Errorf("a fragment without a request: %q, %v", fragment, err)
	}
	bad := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`{{if can "not a permission"}}x{{end}}`)}}, views.Options{})
	if err := bad.RenderRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "page", http.StatusOK, nil); err == nil || !strings.Contains(err.Error(), "can:") {
		t.Errorf("an invalid permission: %v", err)
	}
}
