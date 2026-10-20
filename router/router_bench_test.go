package router_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rocketinsidecorp/forge-go/router"
)

// BenchmarkRouterMatch measures router.Router.ServeHTTP for a request that
// matches one route of a table of 1, 10, 100 or 1000 routes (the middle one),
// answered by a handler that does nothing. Scenario "router_match" in
// docs/BENCHMARKS.md.
//
//	static    GET /static/rN/item
//	param     GET /param/rN/{id}
//	wildcard  GET /files/rN/{path...}
func BenchmarkRouterMatch(b *testing.B) {
	kinds := []struct{ name, pattern, target string }{
		{"static", "GET /static/r%d/item", "/static/r%d/item"},
		{"param", "GET /param/r%d/{id}", "/param/r%d/42"},
		{"wildcard", "GET /files/r%d/{path...}", "/files/r%d/docs/2026/report.txt"},
	}
	for _, kind := range kinds {
		for _, routes := range []int{1, 10, 100, 1000} {
			b.Run(fmt.Sprintf("%s/%d", kind.name, routes), func(b *testing.B) {
				table := router.New()
				for i := range routes {
					if err := table.HandleFunc(fmt.Sprintf(kind.pattern, i), func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }); err != nil {
						b.Fatal(err)
					}
				}
				request := httptest.NewRequest(http.MethodGet, fmt.Sprintf(kind.target, routes/2), nil)
				w := discardResponseWriter{header: http.Header{}}
				b.ReportAllocs()
				for b.Loop() {
					table.ServeHTTP(w, request)
				}
			})
		}
	}
}

type discardResponseWriter struct{ header http.Header }

func (w discardResponseWriter) Header() http.Header          { return w.header }
func (discardResponseWriter) Write(data []byte) (int, error) { return len(data), nil }
func (discardResponseWriter) WriteHeader(int)                {}
