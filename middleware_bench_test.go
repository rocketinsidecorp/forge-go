package forge_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
)

type benchContextKey int

// BenchmarkMiddlewareChain is scenario "middleware_chain" in
// docs/BENCHMARKS.md: the whole Forge handler chain (security headers,
// request ID, recovery, body limit, router) for one GET with 0, 1, 5 or 10
// middleware added by App.Use. "passthrough" middleware only call the next
// handler; "context" middleware also store a value in the request context,
// as most real ones do. The step between 0 and N is the cost of N middleware.
func BenchmarkMiddlewareChain(b *testing.B) {
	kinds := []struct {
		name string
		make func(depth int) func(http.Handler) http.Handler
	}{
		{"passthrough", func(int) func(http.Handler) http.Handler {
			return func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
			}
		}},
		{"context", func(depth int) func(http.Handler) http.Handler {
			return func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), benchContextKey(depth), depth)))
				})
			}
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	for _, kind := range kinds {
		for _, depth := range []int{0, 1, 5, 10} {
			b.Run(fmt.Sprintf("%s/%d", kind.name, depth), func(b *testing.B) {
				app, err := forge.New()
				if err != nil {
					b.Fatal(err)
				}
				if err := app.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }); err != nil {
					b.Fatal(err)
				}
				for i := range depth {
					if err := app.Use(kind.make(i)); err != nil {
						b.Fatal(err)
					}
				}
				handler := app.Handler()
				serve(handler, request) // freezes the chain before the timed loop
				b.ReportAllocs()
				for b.Loop() {
					serve(handler, request)
				}
			})
		}
	}
}
