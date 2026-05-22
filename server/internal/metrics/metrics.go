// Package metrics provides a small, dependency-free metrics primitives
// library: counters, gauges, and histograms with labels.
//
// Why not use prometheus/client_golang directly?
//
//   - The cmd/server already has a hand-rolled metrics aggregator
//     (`serverMetrics` in cmd/server/main.go) that we don't want to
//     replace in a single PR.
//   - We want the *abstraction* the existing code lacks (labels,
//     histograms, gauge semantics) without introducing a transitive
//     dependency yet.
//   - Exporting Prometheus *text format* is straightforward; if the
//     project later adopts client_golang, a Registry adapter is a
//     small, mechanical refactor.
//
// All types are safe for concurrent use.

package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

// Labels is a flat set of key=value pairs attached to a metric series.
// Order is irrelevant; the implementation canonicalises it.
type Labels map[string]string

// canonicalKey returns a stable string for the labels (sorted by key)
// used to identify a series within a metric.
func (l Labels) canonicalKey() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(escapeLabelValue(l[k]))
	}
	return b.String()
}

// promLabels renders the labels in `{k="v",...}` form. Empty when no labels.
func (l Labels) promLabels() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabelValue(v string) string {
	// Prometheus rules: backslash, double-quote, and newline are escaped.
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// ---------------------------------------------------------------------------
// Counter
// ---------------------------------------------------------------------------

// Counter is a monotonically increasing total. Decrement is not
// supported; resetting is not supported either (Prometheus convention).
type Counter struct {
	name string
	help string

	mu     sync.RWMutex
	series map[string]*counterSeries
}

type counterSeries struct {
	labels Labels
	value  uint64 // atomic
}

// NewCounter creates a counter. `name` should follow Prometheus naming
// conventions (snake_case, suffix `_total` recommended).
func NewCounter(name, help string) *Counter {
	return &Counter{name: name, help: help, series: make(map[string]*counterSeries)}
}

// Add increments the series identified by `labels` by `delta`. delta
// must be non-negative — negative increments are silently dropped to
// preserve monotonicity.
func (c *Counter) Add(labels Labels, delta float64) {
	if delta < 0 || c == nil {
		return
	}
	s := c.getOrCreate(labels)
	// Pack delta into uint64 atomic by treating value as fixed-point
	// would lose precision; instead, use a CAS loop on float bits via
	// math.Float64*. To keep the dependency surface tiny we use a
	// simpler approach: integer counters are common, so we widen delta
	// to integer microseconds is overkill — just use mu when delta has
	// a fractional part. For the pure-integer hot path we use
	// atomic.AddUint64 directly.
	if delta == float64(uint64(delta)) {
		atomic.AddUint64(&s.value, uint64(delta))
		return
	}
	c.mu.Lock()
	atomic.StoreUint64(&s.value, atomic.LoadUint64(&s.value)+uint64(delta))
	c.mu.Unlock()
}

// Inc is shorthand for Add(labels, 1).
func (c *Counter) Inc(labels Labels) { c.Add(labels, 1) }

func (c *Counter) getOrCreate(labels Labels) *counterSeries {
	key := labels.canonicalKey()
	c.mu.RLock()
	if s, ok := c.series[key]; ok {
		c.mu.RUnlock()
		return s
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.series[key]; ok {
		return s
	}
	s := &counterSeries{labels: cloneLabels(labels)}
	c.series[key] = s
	return s
}

// ---------------------------------------------------------------------------
// Gauge
// ---------------------------------------------------------------------------

// Gauge is a value that can go up or down. Used for things like
// "currently held leader leases", "open subscriptions", "connections in
// flight".
type Gauge struct {
	name string
	help string

	mu     sync.RWMutex
	series map[string]*gaugeSeries
}

type gaugeSeries struct {
	labels Labels
	mu     sync.Mutex
	value  float64
}

// NewGauge creates a gauge.
func NewGauge(name, help string) *Gauge {
	return &Gauge{name: name, help: help, series: make(map[string]*gaugeSeries)}
}

// Set replaces the gauge value for the labelled series.
func (g *Gauge) Set(labels Labels, v float64) {
	s := g.getOrCreate(labels)
	s.mu.Lock()
	s.value = v
	s.mu.Unlock()
}

// Add adjusts the gauge by delta (may be negative).
func (g *Gauge) Add(labels Labels, delta float64) {
	s := g.getOrCreate(labels)
	s.mu.Lock()
	s.value += delta
	s.mu.Unlock()
}

func (g *Gauge) getOrCreate(labels Labels) *gaugeSeries {
	key := labels.canonicalKey()
	g.mu.RLock()
	if s, ok := g.series[key]; ok {
		g.mu.RUnlock()
		return s
	}
	g.mu.RUnlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.series[key]; ok {
		return s
	}
	s := &gaugeSeries{labels: cloneLabels(labels)}
	g.series[key] = s
	return s
}

// ---------------------------------------------------------------------------
// Histogram
// ---------------------------------------------------------------------------

// Histogram observes values into a fixed set of cumulative buckets. The
// implementation is intentionally minimal: we record bucket counts +
// total sum + total count, in line with Prometheus histogram semantics.
//
// For latency in seconds, sensible defaults are
// {0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}.
type Histogram struct {
	name    string
	help    string
	buckets []float64 // upper bounds, ascending; +Inf is implicit

	mu     sync.RWMutex
	series map[string]*histogramSeries
}

type histogramSeries struct {
	labels  Labels
	mu      sync.Mutex
	counts  []uint64 // length = len(buckets)+1; last is +Inf
	sum     float64
	total   uint64
}

// DefaultLatencyBuckets is a reasonable bucket set for service-call
// latencies expressed in seconds.
var DefaultLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// NewHistogram creates a histogram. `buckets` must be sorted ascending
// and contain at least one finite value. Pass DefaultLatencyBuckets if
// you don't have a strong reason otherwise.
func NewHistogram(name, help string, buckets []float64) *Histogram {
	if len(buckets) == 0 {
		buckets = append([]float64(nil), DefaultLatencyBuckets...)
	}
	bs := append([]float64(nil), buckets...)
	sort.Float64s(bs)
	return &Histogram{
		name: name, help: help, buckets: bs,
		series: make(map[string]*histogramSeries),
	}
}

// Observe records one observation against the labelled series.
func (h *Histogram) Observe(labels Labels, v float64) {
	s := h.getOrCreate(labels)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Find the lowest bucket whose upper bound >= v. Cumulative
	// histogram increments that bucket and every higher one.
	idx := sort.SearchFloat64s(h.buckets, v)
	for i := idx; i < len(s.counts); i++ {
		s.counts[i]++
	}
	s.sum += v
	s.total++
}

func (h *Histogram) getOrCreate(labels Labels) *histogramSeries {
	key := labels.canonicalKey()
	h.mu.RLock()
	if s, ok := h.series[key]; ok {
		h.mu.RUnlock()
		return s
	}
	h.mu.RUnlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.series[key]; ok {
		return s
	}
	s := &histogramSeries{
		labels: cloneLabels(labels),
		counts: make([]uint64, len(h.buckets)+1), // +1 for +Inf
	}
	h.series[key] = s
	return s
}

// ---------------------------------------------------------------------------
// Registry & exposition
// ---------------------------------------------------------------------------

// Registry is the collection of metrics that get rendered together. It
// has no global state; each subsystem can hold its own Registry.
type Registry struct {
	mu         sync.RWMutex
	counters   []*Counter
	gauges     []*Gauge
	histograms []*Histogram
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// MustRegister panics on duplicate registration; matches Prometheus
// client_golang's convention. Duplicate is detected by metric name.
func (r *Registry) MustRegister(m ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{})
	for _, c := range r.counters {
		seen[c.name] = struct{}{}
	}
	for _, g := range r.gauges {
		seen[g.name] = struct{}{}
	}
	for _, h := range r.histograms {
		seen[h.name] = struct{}{}
	}
	for _, item := range m {
		switch t := item.(type) {
		case *Counter:
			if _, dup := seen[t.name]; dup {
				panic("metrics: duplicate registration: " + t.name)
			}
			seen[t.name] = struct{}{}
			r.counters = append(r.counters, t)
		case *Gauge:
			if _, dup := seen[t.name]; dup {
				panic("metrics: duplicate registration: " + t.name)
			}
			seen[t.name] = struct{}{}
			r.gauges = append(r.gauges, t)
		case *Histogram:
			if _, dup := seen[t.name]; dup {
				panic("metrics: duplicate registration: " + t.name)
			}
			seen[t.name] = struct{}{}
			r.histograms = append(r.histograms, t)
		default:
			panic(fmt.Sprintf("metrics: unsupported metric type %T", item))
		}
	}
}

// WritePrometheus renders the registry to w in Prometheus text
// exposition format (version 0.0.4). Idempotent across calls.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, c := range r.counters {
		if err := writeMetricHeader(w, c.name, c.help, "counter"); err != nil {
			return err
		}
		c.mu.RLock()
		keys := sortedKeys(c.series)
		for _, k := range keys {
			s := c.series[k]
			val := atomic.LoadUint64(&s.value)
			if _, err := fmt.Fprintf(w, "%s%s %d\n", c.name, s.labels.promLabels(), val); err != nil {
				c.mu.RUnlock()
				return err
			}
		}
		c.mu.RUnlock()
	}

	for _, g := range r.gauges {
		if err := writeMetricHeader(w, g.name, g.help, "gauge"); err != nil {
			return err
		}
		g.mu.RLock()
		keys := sortedGaugeKeys(g.series)
		for _, k := range keys {
			s := g.series[k]
			s.mu.Lock()
			val := s.value
			s.mu.Unlock()
			if _, err := fmt.Fprintf(w, "%s%s %g\n", g.name, s.labels.promLabels(), val); err != nil {
				g.mu.RUnlock()
				return err
			}
		}
		g.mu.RUnlock()
	}

	for _, h := range r.histograms {
		if err := writeMetricHeader(w, h.name, h.help, "histogram"); err != nil {
			return err
		}
		h.mu.RLock()
		keys := sortedHistogramKeys(h.series)
		for _, k := range keys {
			s := h.series[k]
			s.mu.Lock()
			counts := append([]uint64(nil), s.counts...)
			sum := s.sum
			total := s.total
			s.mu.Unlock()

			for i, ub := range h.buckets {
				le := fmt.Sprintf("%g", ub)
				lbl := withLabel(s.labels, "le", le)
				if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, lbl.promLabels(), counts[i]); err != nil {
					h.mu.RUnlock()
					return err
				}
			}
			lblInf := withLabel(s.labels, "le", "+Inf")
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, lblInf.promLabels(), counts[len(counts)-1]); err != nil {
				h.mu.RUnlock()
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_sum%s %g\n", h.name, s.labels.promLabels(), sum); err != nil {
				h.mu.RUnlock()
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_count%s %d\n", h.name, s.labels.promLabels(), total); err != nil {
				h.mu.RUnlock()
				return err
			}
		}
		h.mu.RUnlock()
	}
	return nil
}

func writeMetricHeader(w io.Writer, name, help, kind string) error {
	if help != "" {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n", name, help); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
	return err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func cloneLabels(l Labels) Labels {
	if len(l) == 0 {
		return nil
	}
	c := make(Labels, len(l))
	for k, v := range l {
		c[k] = v
	}
	return c
}

func withLabel(base Labels, key, value string) Labels {
	out := make(Labels, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[key] = value
	return out
}

func sortedKeys(m map[string]*counterSeries) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedGaugeKeys(m map[string]*gaugeSeries) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedHistogramKeys(m map[string]*histogramSeries) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
