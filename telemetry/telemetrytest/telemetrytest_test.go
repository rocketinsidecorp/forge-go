package telemetrytest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rocketinsidecorp/forge-go/telemetry"
	"github.com/rocketinsidecorp/forge-go/telemetry/telemetrytest"
)

func TestRecorderKeepsSpansAndHelpersReadThem(t *testing.T) {
	t.Parallel()
	tel, recorder := telemetrytest.New(t)
	_, span := tel.Tracer("test").Start(context.Background(), "op", telemetry.SpanKindInternal,
		telemetry.String("name", "value"), telemetry.Int("count", 3))
	span.End()

	spans := recorder.Spans(t, tel)
	if len(spans) != 1 || spans[0].Name != "op" {
		t.Fatalf("spans = %+v", spans)
	}
	// Spans returns what was recorded so far, including earlier spans.
	_, second := tel.Tracer("test").Start(context.Background(), "again", telemetry.SpanKindInternal)
	second.End()
	if got := recorder.Spans(t, tel); len(got) != 2 {
		t.Fatalf("second flush: %d spans, want 2", len(got))
	}
	// The returned slice is a copy.
	spans[0].Name = "changed"
	if recorder.Spans(t, tel)[0].Name != "op" {
		t.Error("Spans exposes the recorder's own slice")
	}

	for key, want := range map[string]string{"name": "value", "count": "3"} {
		if got, ok := telemetrytest.Attribute(spans[0], key); !ok || got != want {
			t.Errorf("Attribute(%q) = %q, %v; want %q", key, got, ok, want)
		}
	}
	if got, ok := telemetrytest.Attribute(spans[0], "missing"); ok || got != "" {
		t.Errorf("a missing attribute reads %q, %v", got, ok)
	}
}

func TestSeriesKeysLabelsInOrderAndCountsHistograms(t *testing.T) {
	t.Parallel()
	tel, _ := telemetrytest.New(t)
	counter, err := tel.Metrics().Counter("test.requests", "", "1", "method", "code")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(2, "GET", "200")
	counter.Add(1, "POST", "500")
	histogram, err := tel.Metrics().Histogram("test.duration", "", "s", nil, "route")
	if err != nil {
		t.Fatal(err)
	}
	histogram.Record(0.1, "/a")
	histogram.Record(0.2, "/a")

	requests := telemetrytest.Series(tel, "test.requests")
	if len(requests) != 2 || requests["method=GET,code=200"] != 2 || requests["method=POST,code=500"] != 1 {
		t.Errorf("counter series = %v", requests)
	}
	if durations := telemetrytest.Series(tel, "test.duration"); len(durations) != 1 || durations["route=/a"] != 2 {
		t.Errorf("histogram series = %v (a histogram reports its count)", durations)
	}
	if got := telemetrytest.Series(tel, "test.unknown"); len(got) != 0 {
		t.Errorf("unknown metric = %v", got)
	}
}

func TestRecorderSamplesEveryTraceOfAnInstrumentedHandler(t *testing.T) {
	t.Parallel()
	tel, recorder := telemetrytest.New(t)
	middleware, err := tel.ServerMiddleware()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if got := len(recorder.Spans(t, tel)); got != 5 {
		t.Fatalf("%d spans, want every one of 5 requests sampled", got)
	}
}
