// Package telemetrytest records spans and reads metrics in tests of
// instrumented code.
package telemetrytest

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Recorder is an in-memory span exporter.
type Recorder struct {
	mu    sync.Mutex
	spans []telemetry.SpanData
}

// ExportSpans keeps spans.
func (r *Recorder) ExportSpans(_ context.Context, _ telemetry.Resource, spans []telemetry.SpanData) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

// Shutdown does nothing.
func (r *Recorder) Shutdown(context.Context) error { return nil }

// New returns a Telemetry that samples every trace and records spans in the
// returned Recorder. It is shut down when the test ends.
func New(t testing.TB) (*telemetry.Telemetry, *Recorder) {
	t.Helper()
	recorder := &Recorder{}
	tel, err := telemetry.New(telemetry.Options{ServiceName: "test", SampleRatio: 1, SpanExporter: recorder})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel, recorder
}

// Spans flushes tel and returns every span recorded so far.
func (r *Recorder) Spans(t testing.TB, tel *telemetry.Telemetry) []telemetry.SpanData {
	t.Helper()
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]telemetry.SpanData(nil), r.spans...)
}

// Attribute returns the value of key on span as text, and whether it is set.
func Attribute(span telemetry.SpanData, key string) (string, bool) {
	for _, attr := range span.Attributes {
		if attr.Key == key {
			return attr.Value.Emit(), true
		}
	}
	return "", false
}

// Series returns the observations of a counter or histogram per series,
// keyed by "label=value" pairs joined with commas in label order: the value
// of a counter, the count of a histogram.
func Series(tel *telemetry.Telemetry, metric string) map[string]float64 {
	series := map[string]float64{}
	for _, data := range tel.Metrics().Collect() {
		if data.Name != metric {
			continue
		}
		for _, point := range data.Points {
			labels := make([]string, len(point.Labels))
			for i, label := range point.Labels {
				labels[i] = label.Key + "=" + label.Value.AsString()
			}
			value := point.Value
			if data.Kind == telemetry.MetricHistogram {
				value = float64(point.Count)
			}
			series[strings.Join(labels, ",")] = value
		}
	}
	return series
}
