package broadcast_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/broadcast"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// The per-principal cap keys streams by the principal auth's middleware
// stores in the request context, which broadcast reads without importing
// auth (ADR 0052): two people behind one address each get their own slots.
func TestHandlerCapsOpenStreamsPerAuthenticatedPrincipal(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{})
	signer, _ := newSigner(t, nil)
	handler, err := broadcast.NewHandler(broadcast.HandlerOptions{Broker: memory, Signer: signer, MaxPerPrincipal: 1, MaxPerTenant: -1})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principals := map[auth.Digest]auth.Principal{}
	bearer := func() string {
		token, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		principal, err := auth.NewPrincipal(uuid.MustNew(), tenant)
		if err != nil {
			t.Fatal(err)
		}
		principals[token.Digest()] = principal
		return "Bearer " + token.Reveal()
	}
	alice, bob := bearer(), bearer()
	middleware, err := auth.NewMiddleware(auth.ResolverFunc(func(_ context.Context, digest auth.Digest) (auth.Session, bool, error) {
		principal, ok := principals[digest]
		return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, ok, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	authenticated := middleware.Authenticate(handler)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authenticated.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	stream, err := signer.Sign(mustGlobal(t, "announcements"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	open := func(authorization string) int {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/streams?stream="+url.QueryEscape(stream), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept", "text/event-stream")
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response.StatusCode
	}
	for _, step := range []struct {
		name, authorization string
		want                int
	}{
		{"alice's first stream", alice, http.StatusOK},
		{"bob's first stream, from the same address", bob, http.StatusOK},
		{"an anonymous stream, counted by address", "", http.StatusOK},
		{"alice's second stream", alice, http.StatusTooManyRequests},
		{"bob's second stream", bob, http.StatusTooManyRequests},
	} {
		if got := open(step.authorization); got != step.want {
			t.Fatalf("%s: status=%d, want %d", step.name, got, step.want)
		}
	}
}
