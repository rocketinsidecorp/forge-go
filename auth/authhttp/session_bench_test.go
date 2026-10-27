package authhttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authhttp"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

type benchWriter struct{ header http.Header }

func (w benchWriter) Header() http.Header { return w.header }

func (benchWriter) Write(data []byte) (int, error) { return len(data), nil }

func (benchWriter) WriteHeader(int) {}

// BenchmarkSessionCookieVerify is scenario "session_cookie_verify" in
// docs/BENCHMARKS.md: Middleware.AuthenticateCookie for a request carrying a
// valid session cookie. Sessions are opaque tokens, so the work is the
// cookie lookup, the token decode, its SHA-256 digest, the session check and
// the principal and tenant context. The session store is an in-memory
// function: a real one adds one database lookup, which the end-to-end
// scenarios measure.
func BenchmarkSessionCookieVerify(b *testing.B) {
	token, err := auth.NewToken()
	if err != nil {
		b.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		b.Fatal(err)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant, "posts:read", "posts:write")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	middleware, err := authhttp.NewMiddleware(auth.ResolverFunc(func(_ context.Context, digest auth.Digest) (auth.Session, bool, error) {
		return auth.Session{Principal: principal, ExpiresAt: now.Add(time.Hour)}, digest == token.Digest(), nil
	}), authhttp.WithClock(func() time.Time { return now }))
	if err != nil {
		b.Fatal(err)
	}
	handler := middleware.AuthenticateCookie(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.FromContext(r.Context()); !ok {
			http.Error(w, "no principal", http.StatusInternalServerError)
		}
	}))
	request := httptest.NewRequest(http.MethodGet, "/posts", nil)
	request.AddCookie(&http.Cookie{Name: authhttp.SessionCookieName, Value: token.Reveal()})
	w := benchWriter{header: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		handler.ServeHTTP(w, request)
	}
}
