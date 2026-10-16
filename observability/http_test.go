package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fabriciobonjorno/forge-go/config"
	"github.com/fabriciobonjorno/forge-go/httpserver"
	"github.com/fabriciobonjorno/forge-go/observability"
	"github.com/fabriciobonjorno/forge-go/router"
	"github.com/fabriciobonjorno/forge-go/web"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestHTTPMiddlewareExtractsTraceContextAndUsesRouteTemplate(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := trace.NewTracerProvider(trace.WithSyncer(exporter))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	routes := router.New()
	var logs bytes.Buffer
	if err := routes.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, request *http.Request) {
		web.Logger(request.Context()).InfoContext(request.Context(), "request handled")
		w.WriteHeader(http.StatusCreated)
	}); err != nil {
		t.Fatal(err)
	}
	server, err := httpserver.NewWithMiddleware(
		config.Default(),
		routes,
		slog.New(slog.NewJSONHandler(&logs, nil)),
		observability.HTTPMiddleware(observability.HTTPConfig{TracerProvider: provider}),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusCreated)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("span count=%d want=1", len(spans))
	}
	span := spans[0]
	if span.Name != "GET /users/{id}" {
		t.Errorf("span name=%q want route template", span.Name)
	}
	if got := span.SpanContext.TraceID().String(); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("trace ID=%q was not extracted from traceparent", got)
	}
	if got := spanAttribute(span.Attributes, "http.route"); got != "/users/{id}" {
		t.Errorf("http.route=%q want /users/{id}; attributes=%+v", got, span.Attributes)
	}
	if got := spanAttribute(span.Attributes, "http.response.status_code"); got != "201" {
		t.Errorf("status attribute=%q want 201", got)
	}
	var logRecord map[string]any
	if err := json.Unmarshal(logs.Bytes(), &logRecord); err != nil {
		t.Fatalf("unmarshal request log: %v; log=%s", err, logs.String())
	}
	if got := logRecord["trace_id"]; got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("request log trace_id=%v", got)
	}
	if got, ok := logRecord["span_id"].(string); !ok || got == "" {
		t.Errorf("request log span_id=%v", logRecord["span_id"])
	}
	if got, ok := logRecord["request_id"].(string); !ok || got == "" || got != recorder.Header().Get("X-Request-ID") {
		t.Errorf("request log request_id=%v; response header=%q", got, recorder.Header().Get("X-Request-ID"))
	}
}

func spanAttribute(attributes []attribute.KeyValue, key string) string {
	for _, item := range attributes {
		if string(item.Key) == key {
			return item.Value.String()
		}
	}
	return ""
}
