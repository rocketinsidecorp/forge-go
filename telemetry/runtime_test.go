package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

func prometheusText(t *testing.T, tel *Telemetry) string {
	t.Helper()
	var out bytes.Buffer
	if err := tel.Metrics().WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestRuntimeMetricsAreExposedInPrometheusFormat(t *testing.T) {
	tel := newTestTelemetry(t, Options{})
	runtime.GC()
	text := prometheusText(t, tel)
	for _, want := range []string{
		"# TYPE go_goroutine_count gauge",
		"# TYPE go_memory_heap_alloc_bytes gauge",
		"# TYPE go_memory_heap_inuse_bytes gauge",
		"# TYPE go_memory_total_bytes gauge",
		"# TYPE go_memory_gc_goal_bytes gauge",
		"# TYPE go_processor_limit gauge",
		"# TYPE go_memory_allocated_bytes_total counter",
		"# TYPE go_gc_cycles_total counter",
		"# TYPE go_gc_pause_duration_seconds histogram",
		`go_gc_pause_duration_seconds_bucket{le="+Inf"}`,
		"go_gc_pause_duration_seconds_count",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics lack %q:\n%s", want, text)
		}
	}
	values := map[string]float64{}
	for _, metric := range tel.Metrics().Collect() {
		if strings.HasPrefix(metric.Name, "go.") && len(metric.Points) > 0 {
			values[metric.Name] = metric.Points[0].Value
			if metric.Kind == MetricHistogram {
				values[metric.Name] = float64(metric.Points[0].Count)
			}
		}
	}
	if values[MetricGoroutines] < 1 || values[MetricGoHeapAlloc] <= 0 || values[MetricGoHeapInuse] < values[MetricGoHeapAlloc] ||
		values[MetricGoMemoryTotal] <= values[MetricGoHeapAlloc] || values[MetricGoProcessors] != float64(runtime.GOMAXPROCS(0)) ||
		values[MetricGoGCCycles] < 1 || values[MetricGoAllocated] <= 0 || values[MetricGoGCPause] < 1 {
		t.Fatalf("runtime values: %v", values)
	}
}

func TestRuntimeMetricsReachOTLP(t *testing.T) {
	c, server := newCollector(t)
	exporter := newExporter(t, server.URL, OTLPOptions{})
	tel := newTestTelemetry(t, Options{MetricExporter: exporter, MetricsInterval: time.Hour})
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := c.received()
	if len(requests) == 0 || requests[0].path != "/v1/metrics" {
		t.Fatalf("requests %+v", requests)
	}
	encoded, _ := json.Marshal(requests[0].payload)
	for _, want := range []string{`"name":"go.goroutine.count"`, `"name":"go.memory.heap.alloc"`, `"name":"go.gc.cycles"`, `"name":"go.gc.pause.duration"`, `"name":"go.memory.allocated"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("OTLP metrics lack %s", want)
		}
	}
}

func TestObservePoolExportsConnectionMetrics(t *testing.T) {
	c, server := newCollector(t)
	exporter := newExporter(t, server.URL, OTLPOptions{})
	tel := newTestTelemetry(t, Options{MetricExporter: exporter, MetricsInterval: time.Hour})
	stats := PoolStats{InUse: 3, Idle: 2, Max: 10, Waits: 7, WaitTime: 1500 * time.Millisecond}
	if err := tel.ObservePool("postgresql", "primary", func() PoolStats { return stats }); err != nil {
		t.Fatal(err)
	}
	text := prometheusText(t, tel)
	for _, want := range []string{
		`db_client_connection_count{db_system_name="postgresql",db_client_connection_pool_name="primary",db_client_connection_state="idle"} 2`,
		`db_client_connection_count{db_system_name="postgresql",db_client_connection_pool_name="primary",db_client_connection_state="used"} 3`,
		`db_client_connection_max{db_system_name="postgresql",db_client_connection_pool_name="primary"} 10`,
		`forge_db_pool_waits_total{db_system_name="postgresql",db_client_connection_pool_name="primary"} 7`,
		`forge_db_pool_wait_duration_seconds_total{db_system_name="postgresql",db_client_connection_pool_name="primary"} 1.5`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics lack %q:\n%s", want, text)
		}
	}
	// Counters never go backwards, gauges follow the pool.
	stats = PoolStats{InUse: 0, Idle: 5, Max: 10, Waits: 3, WaitTime: time.Second}
	text = prometheusText(t, tel)
	if !strings.Contains(text, `forge_db_pool_waits_total{db_system_name="postgresql",db_client_connection_pool_name="primary"} 7`) ||
		!strings.Contains(text, `state="used"} 0`) {
		t.Fatalf("after the pool shrank:\n%s", text)
	}
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(c.received()[0].payload)
	if !strings.Contains(string(encoded), `"name":"forge.db.pool.wait.duration"`) || !strings.Contains(string(encoded), `"asDouble":1.5`) {
		t.Fatalf("OTLP pool metrics: %s", encoded)
	}
	// A second pool under another name adds series; the same name replaces.
	if err := tel.ObservePool("postgresql", "replica", func() PoolStats { return PoolStats{Max: 4} }); err != nil {
		t.Fatal(err)
	}
	if err := tel.ObservePool("postgresql", "primary", func() PoolStats { return PoolStats{Max: 99} }); err != nil {
		t.Fatal(err)
	}
	text = prometheusText(t, tel)
	if !strings.Contains(text, `pool_name="replica"} 4`) || !strings.Contains(text, `pool_name="primary"} 99`) || strings.Contains(text, `pool_name="primary"} 10`) {
		t.Fatalf("pool names:\n%s", text)
	}
	if err := tel.ObservePool("", "x", func() PoolStats { return PoolStats{} }); err == nil {
		t.Fatal("system is required")
	}
	var none *Telemetry
	if err := none.ObservePool("postgresql", "x", nil); err != nil {
		t.Fatal("a nil Telemetry observes nothing")
	}
}

func TestObserversAndObservedCounters(t *testing.T) {
	r := NewRegistry(0)
	calls := 0
	counter, _ := r.Counter("src.total", "d", "{x}", "l")
	float, _ := r.FloatCounter("src.seconds", "d", "s")
	r.Observe("a", func() { calls++; counter.Observe(5, "v"); float.Observe(0.25) })
	r.Observe("panics", func() { panic("bad observer") })
	value := func(name string) float64 {
		for _, metric := range r.Collect() {
			if metric.Name == name {
				return metric.Points[0].Value
			}
		}
		return -1
	}
	if value("src.total") != 5 || value("src.seconds") != 0.25 || calls != 2 {
		t.Fatalf("values %v %v calls %d", value("src.total"), value("src.seconds"), calls)
	}
	counter.Observe(3, "v")
	float.Observe(0.1)
	float.Add(-1)
	float.Observe(nan())
	counter.Add(2, "v")
	if value("src.total") != 7 {
		t.Fatalf("Observe must not lower the counter: %v", value("src.total"))
	}
	r.Observe("a", nil)
	r.Collect()
	if calls != 3 {
		t.Fatalf("removed observer ran: %d", calls)
	}
	if _, err := r.Counter("src.seconds", "d", "s"); err == nil {
		t.Fatal("an integer counter must conflict with the float counter of the same name")
	}
	zero, _ := r.Counter("zero.total", "d", "{x}")
	zero.Observe(0)
	if value("zero.total") != 0 {
		t.Fatal("a first total of zero must create the series")
	}
	var nilRegistry *Registry
	nilRegistry.Observe("x", func() {})
}
