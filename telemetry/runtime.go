package telemetry

import (
	"math"
	"runtime"
	"runtime/metrics"
	"sort"
)

// Runtime metric names. They follow the OpenTelemetry Go runtime
// conventions where one exists (go.goroutine.count, go.memory.allocated,
// go.memory.gc.goal, go.processor.limit) and are read with runtime/metrics,
// which does not stop the world.
const (
	MetricGoroutines    = "go.goroutine.count"
	MetricGoHeapAlloc   = "go.memory.heap.alloc"
	MetricGoHeapInuse   = "go.memory.heap.inuse"
	MetricGoMemoryTotal = "go.memory.total"
	MetricGoGCGoal      = "go.memory.gc.goal"
	MetricGoAllocated   = "go.memory.allocated"
	MetricGoGCCycles    = "go.gc.cycles"
	MetricGoGCPause     = "go.gc.pause.duration"
	MetricGoProcessors  = "go.processor.limit"
)

// gcPauseBounds are the GC pause histogram bounds in seconds: stop-the-world
// pauses are microseconds to a few milliseconds, far below DurationBuckets.
var gcPauseBounds = []float64{1e-5, 2.5e-5, 5e-5, 1e-4, 2.5e-4, 5e-4, 1e-3, 2.5e-3, 5e-3, 1e-2, 2.5e-2, 5e-2, 0.1, 0.25, 0.5, 1}

// Names of the runtime/metrics samples read, in the order of the samples
// slice. The GC pause metric was renamed in Go 1.22; the first that the
// running toolchain supports is used.
const (
	sGoroutines = iota
	sHeapObjects
	sHeapUnused
	sTotal
	sGoal
	sAllocBytes
	sGCCycles
	sPauses
	sPausesLegacy
)

var runtimeSampleNames = [...]string{
	sGoroutines:   "/sched/goroutines:goroutines",
	sHeapObjects:  "/memory/classes/heap/objects:bytes",
	sHeapUnused:   "/memory/classes/heap/unused:bytes",
	sTotal:        "/memory/classes/total:bytes",
	sGoal:         "/gc/heap/goal:bytes",
	sAllocBytes:   "/gc/heap/allocs:bytes",
	sGCCycles:     "/gc/cycles/total:gc-cycles",
	sPauses:       "/sched/pauses/total/gc:seconds",
	sPausesLegacy: "/gc/pauses:seconds",
}

// registerRuntimeMetrics adds the Go runtime instruments to r. They are
// read when the registry is collected (a scrape or an OTLP push), never in a
// request path.
func registerRuntimeMetrics(r *Registry) error {
	gauge := func(name, description, unit string) (*Gauge, error) { return r.Gauge(name, description, unit) }
	goroutines, err := gauge(MetricGoroutines, "Goroutines that currently exist.", "{goroutine}")
	if err != nil {
		return err
	}
	heapAlloc, err := gauge(MetricGoHeapAlloc, "Bytes of allocated heap objects, live and not yet swept.", "By")
	if err != nil {
		return err
	}
	heapInuse, err := gauge(MetricGoHeapInuse, "Bytes in in-use heap spans: heap objects plus their unused space.", "By")
	if err != nil {
		return err
	}
	total, err := gauge(MetricGoMemoryTotal, "Bytes of memory the Go runtime has mapped, in every category.", "By")
	if err != nil {
		return err
	}
	goal, err := gauge(MetricGoGCGoal, "Heap size, in bytes, at which the next garbage collection starts.", "By")
	if err != nil {
		return err
	}
	processors, err := gauge(MetricGoProcessors, "The GOMAXPROCS setting: threads that can execute Go code at once.", "{thread}")
	if err != nil {
		return err
	}
	allocated, err := r.Counter(MetricGoAllocated, "Bytes allocated on the heap since the process started.", "By")
	if err != nil {
		return err
	}
	cycles, err := r.Counter(MetricGoGCCycles, "Completed garbage collection cycles.", "{cycle}")
	if err != nil {
		return err
	}
	pauses, err := r.Histogram(MetricGoGCPause, "Stop-the-world pauses of the garbage collector, re-bucketed from the runtime's finer histogram: a pause counts in the bucket holding the upper edge of its runtime bucket, so one just below a bound can count one bucket high; the sum is estimated from bucket midpoints.", "s", gcPauseBounds)
	if err != nil {
		return err
	}

	samples := make([]metrics.Sample, len(runtimeSampleNames))
	for i, name := range runtimeSampleNames {
		samples[i].Name = name
	}
	counts := make([]uint64, len(gcPauseBounds)+1)
	r.Observe("runtime", func() {
		metrics.Read(samples)
		goroutines.Set(float64(uintSample(samples[sGoroutines])))
		objects, unused := uintSample(samples[sHeapObjects]), uintSample(samples[sHeapUnused])
		heapAlloc.Set(float64(objects))
		heapInuse.Set(float64(objects + unused))
		total.Set(float64(uintSample(samples[sTotal])))
		goal.Set(float64(uintSample(samples[sGoal])))
		processors.Set(float64(runtime.GOMAXPROCS(0)))
		allocated.Observe(clampInt64(uintSample(samples[sAllocBytes])))
		cycles.Observe(clampInt64(uintSample(samples[sGCCycles])))
		for _, index := range [...]int{sPauses, sPausesLegacy} {
			if samples[index].Value.Kind() != metrics.KindFloat64Histogram {
				continue
			}
			sum := foldHistogram(samples[index].Value.Float64Histogram(), gcPauseBounds, counts)
			pauses.set(counts, sum)
			break
		}
	})
	return nil
}

func uintSample(sample metrics.Sample) uint64 {
	if sample.Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample.Value.Uint64()
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// foldHistogram re-buckets h into counts (len(bounds)+1) by each source
// bucket's upper edge, and returns the sum estimated from bucket midpoints
// (the finite edge for open-ended buckets).
func foldHistogram(h *metrics.Float64Histogram, bounds []float64, counts []uint64) float64 {
	clear(counts)
	var sum float64
	for i, count := range h.Counts {
		if count == 0 {
			continue
		}
		lo, hi := h.Buckets[i], h.Buckets[i+1]
		counts[sort.SearchFloat64s(bounds, hi)] += count
		switch {
		case math.IsInf(lo, -1):
			sum += float64(count) * hi
		case math.IsInf(hi, 1):
			sum += float64(count) * lo
		default:
			sum += float64(count) * (lo + hi) / 2
		}
	}
	return sum
}
