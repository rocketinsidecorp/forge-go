package router_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rocketinsidecorp/forge-go/router"
)

func BenchmarkRouterStatic(b *testing.B) {
	routes := router.New()
	if err := routes.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }); err != nil {
		b.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	b.ReportAllocs()
	for b.Loop() {
		routes.ServeHTTP(discardResponseWriter{}, request)
	}
}

type discardResponseWriter struct{}

func (discardResponseWriter) Header() http.Header            { return make(http.Header) }
func (discardResponseWriter) Write(data []byte) (int, error) { return len(data), nil }
func (discardResponseWriter) WriteHeader(int)                {}
