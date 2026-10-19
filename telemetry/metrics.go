package telemetry

import (
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metric limits. Every instrument declares its label names up front, and
// each distinct combination of label values is one series. The series limit
// keeps a label fed by untrusted input from exhausting memory or the
// metrics backend: once an instrument reaches it, new combinations are
// folded into one series whose labels are all OverflowLabelValue.
const (
	MaxMetricLabels     = 6
	DefaultMaxSeries    = 1000
	MaxLabelValueLength = 128
	OverflowLabelValue  = "_overflow"
	overflowMetricName  = "forge.telemetry.metric.overflows"
)

// DurationBuckets are the histogram bounds, in seconds, OpenTelemetry
// semantic conventions advise for request and operation durations.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

var (
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_./-]{0,254}$`)
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]{0,127}$`)
)

// MetricKind is the type of an instrument.
type MetricKind uint8

const (
	// MetricCounter is a monotonic sum.
	MetricCounter MetricKind = iota + 1
	// MetricGauge is a value that goes up and down, such as requests in
	// flight.
	MetricGauge
	// MetricHistogram is a distribution in explicit buckets.
	MetricHistogram
)

type labelValues [MaxMetricLabels]string

// Registry holds instruments. It is safe for concurrent use. A nil *Registry
// returns nil instruments, whose methods do nothing.
type Registry struct {
	maxSeries int
	start     time.Time

	mu          sync.Mutex
	instruments map[string]*instrument
	overflows   *Counter
}

// NewRegistry returns an empty registry. maxSeries bounds the series of each
// instrument; zero or less means DefaultMaxSeries.
func NewRegistry(maxSeries int) *Registry {
	if maxSeries <= 0 {
		maxSeries = DefaultMaxSeries
	}
	r := &Registry{maxSeries: maxSeries, start: time.Now(), instruments: make(map[string]*instrument)}
	overflows, err := r.Counter(overflowMetricName, "Measurements folded into an overflow series because an instrument reached its series limit.", "{measurement}")
	if err != nil {
		panic(err) // a constant, valid definition
	}
	r.overflows = overflows
	return r
}

type instrument struct {
	registry    *Registry
	name        string
	description string
	unit        string
	kind        MetricKind
	labels      []string
	bounds      []float64

	mu       sync.RWMutex
	series   map[labelValues]*point
	overflow *point
}

type point struct {
	labels labelValues
	start  time.Time
	// value holds a counter's count, or a gauge's float64 bits.
	value atomic.Uint64
	// counts are histogram bucket counts, len(bounds)+1; sum is float64 bits.
	counts []atomic.Uint64
	sum    atomic.Uint64
}

// Counter is a monotonic count, such as requests served.
type Counter struct{ instrument *instrument }

// Gauge is a value that can go up and down, such as requests in flight.
type Gauge struct{ instrument *instrument }

// Histogram records a distribution, such as request durations.
type Histogram struct{ instrument *instrument }

// Counter returns the counter named name, creating it on first use.
// Registering the same name again with an identical definition returns the
// same counter, so two components can share it; a conflicting definition is
// an error.
func (r *Registry) Counter(name, description, unit string, labels ...string) (*Counter, error) {
	in, err := r.register(name, description, unit, MetricCounter, nil, labels)
	if in == nil {
		return nil, err
	}
	return &Counter{instrument: in}, err
}

// Gauge returns the gauge named name; see Counter.
func (r *Registry) Gauge(name, description, unit string, labels ...string) (*Gauge, error) {
	in, err := r.register(name, description, unit, MetricGauge, nil, labels)
	if in == nil {
		return nil, err
	}
	return &Gauge{instrument: in}, err
}

// Histogram returns the histogram named name with the given ascending
// bucket bounds (nil means DurationBuckets); see Counter.
func (r *Registry) Histogram(name, description, unit string, bounds []float64, labels ...string) (*Histogram, error) {
	if bounds == nil {
		bounds = DurationBuckets
	}
	in, err := r.register(name, description, unit, MetricHistogram, bounds, labels)
	if in == nil {
		return nil, err
	}
	return &Histogram{instrument: in}, err
}

func (r *Registry) register(name, description, unit string, kind MetricKind, bounds []float64, labels []string) (*instrument, error) {
	if r == nil {
		return nil, nil
	}
	if !metricNamePattern.MatchString(name) {
		return nil, fmt.Errorf("telemetry: invalid metric name %q", name)
	}
	if len(labels) > MaxMetricLabels {
		return nil, fmt.Errorf("telemetry: metric %s has more than %d labels", name, MaxMetricLabels)
	}
	for i, label := range labels {
		if !labelNamePattern.MatchString(label) || slices.Contains(labels[:i], label) || label == "le" {
			return nil, fmt.Errorf("telemetry: metric %s has an invalid or duplicate label %q", name, label)
		}
	}
	for i, bound := range bounds {
		if math.IsNaN(bound) || math.IsInf(bound, 0) || (i > 0 && bound <= bounds[i-1]) {
			return nil, fmt.Errorf("telemetry: histogram %s bounds must be finite and strictly ascending", name)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.instruments[name]; ok {
		if existing.kind != kind || existing.unit != unit || !slices.Equal(existing.labels, labels) || !slices.Equal(existing.bounds, bounds) {
			return nil, fmt.Errorf("telemetry: metric %s is already registered with a different definition", name)
		}
		return existing, nil
	}
	in := &instrument{
		registry:    r,
		name:        name,
		description: description,
		unit:        unit,
		kind:        kind,
		labels:      slices.Clone(labels),
		bounds:      slices.Clone(bounds),
		series:      make(map[labelValues]*point),
	}
	r.instruments[name] = in
	return in, nil
}

// point returns the series for values, creating it if the instrument is
// below its series limit. The lookup of an existing series does not
// allocate.
func (in *instrument) point(values []string) *point {
	if len(values) != len(in.labels) {
		// A programming error; counting it as overflow keeps it visible
		// without panicking in a request path.
		return in.overflowPoint()
	}
	var key labelValues
	for i, value := range values {
		if len(value) > MaxLabelValueLength {
			value = value[:MaxLabelValueLength]
		}
		key[i] = value
	}
	in.mu.RLock()
	p := in.series[key]
	in.mu.RUnlock()
	if p != nil {
		return p
	}
	in.mu.Lock()
	if p = in.series[key]; p != nil {
		in.mu.Unlock()
		return p
	}
	if len(in.series) >= in.registry.maxSeries {
		in.mu.Unlock()
		return in.overflowPoint()
	}
	for i := range len(values) {
		// Detach from the caller's memory: a label may be a substring of a
		// large request buffer.
		key[i] = strings.Clone(key[i])
	}
	p = in.newPoint(key)
	in.series[key] = p
	in.mu.Unlock()
	return p
}

func (in *instrument) overflowPoint() *point {
	if in.registry.overflows != nil && in.name != overflowMetricName {
		in.registry.overflows.Add(1)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.overflow == nil {
		var key labelValues
		for i := range in.labels {
			key[i] = OverflowLabelValue
		}
		in.overflow = in.newPoint(key)
	}
	return in.overflow
}

func (in *instrument) newPoint(key labelValues) *point {
	p := &point{labels: key, start: time.Now()}
	if in.kind == MetricHistogram {
		p.counts = make([]atomic.Uint64, len(in.bounds)+1)
	}
	return p
}

// Add increases the counter by n for the given label values, which must
// match the instrument's label names in number and order. Negative n is
// ignored.
func (c *Counter) Add(n int64, values ...string) {
	if c == nil || n <= 0 {
		return
	}
	c.instrument.point(values).value.Add(uint64(n))
}

// Set sets the gauge for the given label values.
func (g *Gauge) Set(v float64, values ...string) {
	if g == nil || math.IsNaN(v) {
		return
	}
	g.instrument.point(values).value.Store(math.Float64bits(v))
}

// Add changes the gauge by delta for the given label values.
func (g *Gauge) Add(delta float64, values ...string) {
	if g == nil || math.IsNaN(delta) {
		return
	}
	addFloat(&g.instrument.point(values).value, delta)
}

// Record adds v to the histogram for the given label values.
func (h *Histogram) Record(v float64, values ...string) {
	if h == nil || math.IsNaN(v) {
		return
	}
	p := h.instrument.point(values)
	index, _ := slices.BinarySearch(h.instrument.bounds, v)
	p.counts[index].Add(1)
	addFloat(&p.sum, v)
}

func addFloat(target *atomic.Uint64, delta float64) {
	for {
		old := target.Load()
		if target.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+delta)) {
			return
		}
	}
}

// MetricData is a snapshot of one instrument.
type MetricData struct {
	Name        string
	Description string
	Unit        string
	Kind        MetricKind
	// Bounds are a histogram's bucket upper bounds.
	Bounds []float64
	Points []PointData
}

// PointData is a snapshot of one series. Counters and gauges set Value;
// histograms set Count, Sum and BucketCounts (len(Bounds)+1, the last for
// values above every bound).
type PointData struct {
	Labels       []Attribute
	Start        time.Time
	Time         time.Time
	Value        float64
	Count        uint64
	Sum          float64
	BucketCounts []uint64
}

// Collect returns a snapshot of every instrument with at least one series,
// sorted by name, with series sorted by label values. Counter and histogram
// values are cumulative since the series was created.
func (r *Registry) Collect() []MetricData {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.mu.Lock()
	instruments := make([]*instrument, 0, len(r.instruments))
	for _, in := range r.instruments {
		instruments = append(instruments, in)
	}
	r.mu.Unlock()
	slices.SortFunc(instruments, func(a, b *instrument) int { return cmp.Compare(a.name, b.name) })

	result := make([]MetricData, 0, len(instruments))
	for _, in := range instruments {
		in.mu.RLock()
		points := make([]*point, 0, len(in.series)+1)
		for _, p := range in.series {
			points = append(points, p)
		}
		if in.overflow != nil {
			points = append(points, in.overflow)
		}
		in.mu.RUnlock()
		if len(points) == 0 {
			continue
		}
		slices.SortFunc(points, func(a, b *point) int { return slices.Compare(a.labels[:], b.labels[:]) })
		data := MetricData{Name: in.name, Description: in.description, Unit: in.unit, Kind: in.kind, Bounds: in.bounds, Points: make([]PointData, 0, len(points))}
		for _, p := range points {
			data.Points = append(data.Points, in.snapshot(p, now))
		}
		result = append(result, data)
	}
	return result
}

func (in *instrument) snapshot(p *point, now time.Time) PointData {
	labels := make([]Attribute, len(in.labels))
	for i, name := range in.labels {
		labels[i] = String(name, p.labels[i])
	}
	data := PointData{Labels: labels, Start: p.start, Time: now}
	switch in.kind {
	case MetricCounter:
		data.Value = float64(p.value.Load())
	case MetricGauge:
		data.Value = math.Float64frombits(p.value.Load())
	case MetricHistogram:
		data.BucketCounts = make([]uint64, len(p.counts))
		for i := range p.counts {
			data.BucketCounts[i] = p.counts[i].Load()
			data.Count += data.BucketCounts[i]
		}
		data.Sum = math.Float64frombits(p.sum.Load())
	}
	return data
}
