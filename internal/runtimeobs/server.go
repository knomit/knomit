// Package runtimeobs serves the optional runtime diagnostics port: live
// introspection and process controls (/runtime/*) alongside pprof, expvar, and
// Prometheus-text metrics (/debug/*, /metrics). It is bound to a local address
// and off unless explicitly enabled, so it carries zero steady-state cost and
// is never reachable from the public API. Pure stdlib.
package runtimeobs

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"maps"
	"net/http"
	nhpprof "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"knomit/internal/metrics"
)

// Options configures the diagnostics server.
type Options struct {
	// StartedAt anchors the reported uptime. Zero means "now".
	StartedAt time.Time
	// StatusExtra contributes process-specific fields (repos, pool stats, jobs,
	// sessions) to /runtime/status. It is injected by the caller to avoid a
	// dependency from this package onto app/repos. May be nil.
	StatusExtra func() map[string]any
	// HeapDumpDir is where /runtime/heapdump writes heap profiles.
	HeapDumpDir string
	// BackupStatus reports replication state for every database knomit believes
	// is being replicated. It is injected, and speaks the local BackupDBStatus
	// mirror type, so this package never imports internal/backup.
	//
	// Nil means backup is disabled — the /runtime/status block is then OMITTED
	// rather than rendered as null, and no knomit_backup_* series are emitted.
	// A disabled feature should be absent from the surface, not present and
	// empty, which reads as broken.
	//
	// The hook is expensive by construction (see defaultBackupStatusTTL); its
	// result is cached for BackupStatusTTL and shared by /metrics and
	// /runtime/status.
	BackupStatus func(context.Context) []BackupDBStatus
	// BackupStatusTTL bounds how often BackupStatus is actually called.
	// Non-positive means defaultBackupStatusTTL — never "no cache".
	BackupStatusTTL time.Duration

	// now is the clock the backup status cache ages against. Tests replace it;
	// nothing in production sets it.
	now func() time.Time
}

// Server holds the diagnostics mux.
type Server struct {
	opts    Options
	metrics *metrics.Registry
	// backup caches the replication-status probe. Nil when backup is disabled.
	backup *backupStatusCache
}

// NewServer returns a diagnostics Server. /metrics renders the process-global
// metrics.Default registry, which any subsystem records into directly — so the
// numbers exist whether or not this port is ever enabled. The same registry is
// also published as the "knomit" expvar var (one source, two renderers:
// Prometheus text on /metrics, JSON on /debug/vars).
func NewServer(opts Options) *Server {
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now()
	}
	publishMetricsExpvar.Do(func() {
		expvar.Publish("knomit", expvar.Func(func() any { return metrics.Default().Snapshot() }))
	})
	s := &Server{opts: opts, metrics: metrics.Default()}
	if opts.BackupStatus != nil {
		ttl := opts.BackupStatusTTL
		if ttl <= 0 {
			ttl = defaultBackupStatusTTL
		}
		now := opts.now
		if now == nil {
			now = time.Now
		}
		s.backup = &backupStatusCache{fetch: opts.BackupStatus, ttl: ttl, now: now}
	}
	return s
}

// publishMetricsExpvar guards the one-time expvar.Publish (which panics on a
// duplicate name).
var publishMetricsExpvar sync.Once

// Handler builds the diagnostics mux: /runtime/* controls, /debug/pprof/*,
// /debug/vars (expvar), and /metrics. It mounts pprof explicitly rather than
// relying on the http.DefaultServeMux side-effect registration, so these
// endpoints exist ONLY on this gated port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/runtime/status", s.handleStatus)
	mux.HandleFunc("/runtime/loglevel", s.handleLogLevel)
	mux.HandleFunc("/runtime/profile/mutex", s.handleProfileMutex)
	mux.HandleFunc("/runtime/profile/block", s.handleProfileBlock)
	mux.HandleFunc("/runtime/gc", s.handleGC)
	mux.HandleFunc("/runtime/heapdump", s.handleHeapDump)

	mux.HandleFunc("/debug/pprof/", nhpprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", nhpprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", nhpprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", nhpprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", nhpprof.Trace)
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/metrics", s.handleMetrics)

	return mux
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Resolved BEFORE a byte is written. The backup status is the only part of
	// this response that can block (a cold cache waits for the first probe), and
	// doing it mid-body would abandon a half-written exposition — losing every
	// process metric to a delay that has nothing to do with them.
	backup := s.backupStatus(r.Context())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.WriteProm(w)
	// Appended rather than registered as gauges: the backup series are LABELLED
	// per database and the set of databases changes at runtime (repos created,
	// archived, purged), which the registry's fixed-name gauges cannot express.
	writeBackupMetrics(w, backup)
}

func (s *Server) handleHeapDump(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.HeapDumpDir == "" {
		http.Error(w, "heap dump directory not configured", http.StatusServiceUnavailable)
		return
	}
	if err := os.MkdirAll(s.opts.HeapDumpDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("heap-%s.pprof", time.Now().UTC().Format("20060102T150405.000Z"))
	path := filepath.Join(s.opts.HeapDumpDir, name)
	f, err := os.Create(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	runtime.GC() // get up-to-date allocation statistics in the profile
	if err := pprof.WriteHeapProfile(f); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"path": path})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	status := map[string]any{
		"uptime":     time.Since(s.opts.StartedAt).Round(time.Second).String(),
		"started_at": s.opts.StartedAt.UTC().Format(time.RFC3339),
		"goroutines": runtime.NumGoroutine(),
		"go_version": runtime.Version(),
		"mem_alloc":  m.Alloc,
		"mem_sys":    m.Sys,
		"num_gc":     m.NumGC,
		"gomaxprocs": runtime.GOMAXPROCS(0),
		"num_cpu":    runtime.NumCPU(),
	}
	if s.opts.StatusExtra != nil {
		maps.Copy(status, s.opts.StatusExtra())
	}
	if s.backup != nil {
		// Normalised to a non-nil slice: zero tracked databases is a valid state
		// (knomit creates no repo on its own), and it must render as [] rather
		// than null so a consumer can range over it without special-casing.
		// Absent still means "backup is off" — see Options.BackupStatus.
		dbs := s.backupStatus(r.Context())
		if dbs == nil {
			dbs = []BackupDBStatus{}
		}
		status["backup"] = dbs
	}
	writeJSON(w, status)
}

func (s *Server) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"level": zerolog.GlobalLevel().String()})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lvl, err := zerolog.ParseLevel(r.URL.Query().Get("level"))
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid level: %v", err), http.StatusBadRequest)
		return
	}
	zerolog.SetGlobalLevel(lvl)
	writeJSON(w, map[string]any{"level": lvl.String()})
}

func (s *Server) handleProfileMutex(w http.ResponseWriter, r *http.Request) {
	rate, ok := postRate(w, r)
	if !ok {
		return
	}
	runtime.SetMutexProfileFraction(rate)
	writeJSON(w, map[string]any{"mutex_profile_fraction": rate})
}

func (s *Server) handleProfileBlock(w http.ResponseWriter, r *http.Request) {
	rate, ok := postRate(w, r)
	if !ok {
		return
	}
	runtime.SetBlockProfileRate(rate)
	writeJSON(w, map[string]any{"block_profile_rate": rate})
}

func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runtime.GC()
	writeJSON(w, map[string]any{"gc": "done"})
}

// postRate parses ?rate=N from a POST request. A rate of 0 disables the
// profiler (the default, zero-cost state).
func postRate(w http.ResponseWriter, r *http.Request) (int, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return 0, false
	}
	rate, err := strconv.Atoi(r.URL.Query().Get("rate"))
	if err != nil {
		http.Error(w, "rate must be an integer", http.StatusBadRequest)
		return 0, false
	}
	return rate, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
