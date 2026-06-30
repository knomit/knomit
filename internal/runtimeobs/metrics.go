package runtimeobs

import (
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
)

// Registry is a minimal, dependency-free metrics registry rendering the
// Prometheus text exposition format. It is the single source of truth behind
// /metrics; counters and gauges are integers (sufficient for request counts,
// goroutines, bytes, and GC cycles). Histograms are intentionally omitted here
// — the two that matter (embed inference latency, SQLite lock-wait) need hooks
// inside store/embeddings and are tracked as follow-up instrumentation.
type Registry struct {
	mu         sync.Mutex
	counters   []*Counter
	gauges     []*Gauge
	collectors []func()
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Counter is a monotonically increasing integer metric.
type Counter struct {
	name, help string
	v          atomic.Int64
}

// Inc increments the counter by one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add increases the counter by n.
func (c *Counter) Add(n int64) { c.v.Add(n) }

// Gauge is an integer metric that can go up or down.
type Gauge struct {
	name, help string
	v          atomic.Int64
}

// Set replaces the gauge value.
func (g *Gauge) Set(n int64) { g.v.Store(n) }

// NewCounter registers and returns a counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	c := &Counter{name: name, help: help}
	r.mu.Lock()
	r.counters = append(r.counters, c)
	r.mu.Unlock()
	return c
}

// NewGauge registers and returns a gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	g := &Gauge{name: name, help: help}
	r.mu.Lock()
	r.gauges = append(r.gauges, g)
	r.mu.Unlock()
	return g
}

// AddCollector registers a function run immediately before each render, used to
// refresh gauges from live runtime state.
func (r *Registry) AddCollector(fn func()) {
	r.mu.Lock()
	r.collectors = append(r.collectors, fn)
	r.mu.Unlock()
}

// WriteProm renders all metrics in the Prometheus text exposition format.
func (r *Registry) WriteProm(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, fn := range r.collectors {
		fn()
	}
	for _, c := range r.counters {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", c.name, c.help, c.name, c.name, c.v.Load())
	}
	for _, g := range r.gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", g.name, g.help, g.name, g.name, g.v.Load())
	}
}

// withRuntimeGauges seeds a registry with always-present process gauges so
// /metrics is useful even before any app-specific instrumentation exists.
func withRuntimeGauges(r *Registry) *Registry {
	goroutines := r.NewGauge("knomit_goroutines", "Number of live goroutines.")
	allocBytes := r.NewGauge("knomit_mem_alloc_bytes", "Bytes of allocated heap currently in use.")
	sysBytes := r.NewGauge("knomit_mem_sys_bytes", "Total bytes of memory obtained from the OS.")
	gcTotal := r.NewGauge("knomit_gc_total", "Completed GC cycles.")
	r.AddCollector(func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		goroutines.Set(int64(runtime.NumGoroutine()))
		allocBytes.Set(int64(m.Alloc))
		sysBytes.Set(int64(m.Sys))
		gcTotal.Set(int64(m.NumGC))
	})
	return r
}
