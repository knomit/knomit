// Package metrics is a tiny, dependency-free metrics registry that renders the
// Prometheus text exposition format. It is a leaf package (stdlib only) so any
// subsystem — store, web, embeddings — can record into the process-global
// Default registry without importing the diagnostics server. Recording costs an
// atomic add and happens whether or not the /metrics port is ever scraped.
package metrics

import (
	"fmt"
	"io"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metrics and renders them as Prometheus text.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	vecs       map[string]*CounterVec
	hists      map[string]*Histogram
	order      []string // stable render order by metric name
	collectors []func()
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		counters: map[string]*Counter{},
		gauges:   map[string]*Gauge{},
		vecs:     map[string]*CounterVec{},
		hists:    map[string]*Histogram{},
	}
}

// Counter is a monotonically increasing integer metric.
type Counter struct {
	name, help string
	v          atomic.Int64
}

func (c *Counter) Inc()         { c.v.Add(1) }
func (c *Counter) Add(n int64)  { c.v.Add(n) }
func (c *Counter) Value() int64 { return c.v.Load() }

// Gauge is an integer metric that can move up or down.
type Gauge struct {
	name, help string
	v          atomic.Int64
}

func (g *Gauge) Set(n int64) { g.v.Store(n) }
func (g *Gauge) Add(n int64) { g.v.Add(n) }

// CounterVec is a counter partitioned by label values.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]*Counter // key: joined label values
	keyvals    map[string][]string // key -> label values for rendering
}

// With returns the counter for a specific set of label values (in the order the
// labels were declared). Missing series are created on first use.
func (cv *CounterVec) With(values ...string) *Counter {
	key := strings.Join(values, "\x00")
	cv.mu.Lock()
	defer cv.mu.Unlock()
	c, ok := cv.series[key]
	if !ok {
		c = &Counter{name: cv.name}
		cv.series[key] = c
		cv.keyvals[key] = append([]string(nil), values...)
	}
	return c
}

// Histogram buckets observations into fixed cumulative buckets (Prometheus
// convention) plus _sum and _count.
type Histogram struct {
	name, help string
	bounds     []float64
	counts     []atomic.Int64 // per-bucket (non-cumulative); rendered cumulatively
	inf        atomic.Int64
	sumBits    atomic.Uint64 // float64 sum via bit-pattern CAS
	count      atomic.Int64
}

// Observe records a value.
func (h *Histogram) Observe(v float64) {
	h.count.Add(1)
	addFloat(&h.sumBits, v)
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i].Add(1)
			return
		}
	}
	h.inf.Add(1)
}

// Count returns the total number of observations.
func (h *Histogram) Count() int64 { return h.count.Load() }

func addFloat(bits *atomic.Uint64, v float64) {
	for {
		old := bits.Load()
		nw := math.Float64bits(math.Float64frombits(old) + v)
		if bits.CompareAndSwap(old, nw) {
			return
		}
	}
}

// Counter returns the named counter, creating it once.
func (r *Registry) Counter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{name: name, help: help}
	r.counters[name] = c
	r.order = append(r.order, name)
	return c
}

// Gauge returns the named gauge, creating it once.
func (r *Registry) Gauge(name, help string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &Gauge{name: name, help: help}
	r.gauges[name] = g
	r.order = append(r.order, name)
	return g
}

// CounterVec returns the named labeled counter, creating it once.
func (r *Registry) CounterVec(name, help string, labels ...string) *CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cv, ok := r.vecs[name]; ok {
		return cv
	}
	cv := &CounterVec{name: name, help: help, labels: labels, series: map[string]*Counter{}, keyvals: map[string][]string{}}
	r.vecs[name] = cv
	r.order = append(r.order, name)
	return cv
}

// Histogram returns the named histogram, creating it once. bounds must be
// ascending upper bucket boundaries.
func (r *Registry) Histogram(name, help string, bounds []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hists[name]; ok {
		return h
	}
	h := &Histogram{name: name, help: help, bounds: bounds, counts: make([]atomic.Int64, len(bounds))}
	r.hists[name] = h
	r.order = append(r.order, name)
	return h
}

// AddCollector registers a function run immediately before each render, to
// refresh gauges from live runtime state.
func (r *Registry) AddCollector(fn func()) {
	r.mu.Lock()
	r.collectors = append(r.collectors, fn)
	r.mu.Unlock()
}

// WriteProm renders every metric in the Prometheus text exposition format.
func (r *Registry) WriteProm(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, fn := range r.collectors {
		fn()
	}
	for _, name := range r.order {
		switch {
		case r.counters[name] != nil:
			c := r.counters[name]
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, c.help, name, name, c.v.Load())
		case r.gauges[name] != nil:
			g := r.gauges[name]
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, g.help, name, name, g.v.Load())
		case r.vecs[name] != nil:
			writeVec(w, r.vecs[name])
		case r.hists[name] != nil:
			writeHist(w, r.hists[name])
		}
	}
}

func writeVec(w io.Writer, cv *CounterVec) {
	cv.mu.Lock()
	defer cv.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", cv.name, cv.help, cv.name)
	keys := make([]string, 0, len(cv.series))
	for k := range cv.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var b strings.Builder
		for i, lbl := range cv.labels {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `%s=%q`, lbl, cv.keyvals[k][i])
		}
		fmt.Fprintf(w, "%s{%s} %d\n", cv.name, b.String(), cv.series[k].v.Load())
	}
}

func writeHist(w io.Writer, h *Histogram) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	var cumulative int64
	for i, b := range h.bounds {
		cumulative += h.counts[i].Load()
		fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", h.name, trimFloat(b), cumulative)
	}
	cumulative += h.inf.Load()
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, cumulative)
	fmt.Fprintf(w, "%s_sum %s\n", h.name, strconv.FormatFloat(math.Float64frombits(h.sumBits.Load()), 'g', -1, 64))
	fmt.Fprintf(w, "%s_count %d\n", h.name, h.count.Load())
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

var (
	defaultOnce sync.Once
	defaultReg  *Registry
)

// Default is the process-global registry rendered by the /metrics endpoint. It
// is seeded with always-present runtime gauges on first use.
func Default() *Registry {
	defaultOnce.Do(func() {
		defaultReg = NewRegistry()
		registerRuntimeGauges(defaultReg)
	})
	return defaultReg
}

func registerRuntimeGauges(r *Registry) {
	goroutines := r.Gauge("knomit_goroutines", "Number of live goroutines.")
	allocBytes := r.Gauge("knomit_mem_alloc_bytes", "Bytes of allocated heap in use.")
	sysBytes := r.Gauge("knomit_mem_sys_bytes", "Total bytes obtained from the OS.")
	gcTotal := r.Gauge("knomit_gc_total", "Completed GC cycles.")
	r.AddCollector(func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		goroutines.Set(int64(runtime.NumGoroutine()))
		allocBytes.Set(int64(m.Alloc))
		sysBytes.Set(int64(m.Sys))
		gcTotal.Set(int64(m.NumGC))
	})
}
