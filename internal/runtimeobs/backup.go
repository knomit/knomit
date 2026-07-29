package runtimeobs

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// BackupDBStatus mirrors backup.DBStatus. It is declared locally, and copied
// by the caller, so this package stays dependency-free in the direction of the
// app: runtimeobs must never import internal/backup, which would drag the
// replication client into every consumer of the diagnostics port.
//
// The fields carry the meanings backup.Manager.Status works to produce, and
// rendering must preserve them rather than flatten them:
//
//   - LastError on an entry with zero TXIDs is the RECONCILED case — knomit
//     believes this database is being replicated and the agent has never heard
//     of it, or the agent is unreachable. Either way it is NOT being backed up,
//     and the text says which.
//   - Paused means a store swap untracked it deliberately. It is not an error
//     and not healthy; it is a state that must be visible, because a paused
//     database that is never resumed stops being backed up silently.
//   - LastSyncUnix is zero when the database has NEVER synced. Zero is not a
//     timestamp — see writeBackupMetrics.
type BackupDBStatus struct {
	Name         string `json:"name"`
	LocalTXID    uint64 `json:"local_txid"`
	RemoteTXID   uint64 `json:"remote_txid"`
	InSync       bool   `json:"in_sync"`
	LastSyncUnix int64  `json:"last_sync_unix,omitempty"`
	Paused       bool   `json:"paused,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// defaultBackupStatusTTL is how long a replication-status probe is reused when
// the caller configures no TTL of its own.
//
// A TTL is MANDATORY here, not an optimisation. backup.Manager.Status is a live
// probe by design and deliberately does not cache: each call costs one remote
// LTX listing per tracked database, drained in full with no pagination limit,
// and the tracked set includes every archived database — so the cost scales
// with the archive count, not the repo count. Wiring that straight into
// /metrics would make a Prometheus scrape a load generator against the object
// store, and a second consumer (a dashboard polling /runtime/status) would
// double it.
//
// 15s is chosen against what the number is FOR rather than against any one
// scrape interval, which is the point: the cache bounds the probe rate to
// 1/TTL however many consumers poll and however often. 15s is short enough that
// the lag gauge still moves within a single alert evaluation window, and long
// enough that the common 15-30s scrape pays for at most one probe per scrape.
// Deployments with many archives raise it (backup.status_cache_ttl).
const defaultBackupStatusTTL = 15 * time.Second

// backupStatusCache reuses one replication-status probe across every consumer
// of the diagnostics port for up to ttl.
//
// # The probe is detached from whoever triggered it
//
// This is the property the whole cache rests on, and the obvious
// implementation — call fetch on the request's context — destroys it. net/http
// cancels a request context when the client disconnects, which is exactly what
// Prometheus does at scrape_timeout (10s by default), while the status round
// trip is allowed two minutes. So on the ONE deployment this cache exists for —
// enough archived databases that a probe outlives a scrape timeout — every
// probe would be cancelled, nothing would ever be stored, and each consumer
// would re-probe on its own schedule. The documented guarantee would quietly
// become false precisely when it mattered.
//
// Caching a CANCELLED answer is not the alternative: Manager.Status reports
// every tracked database as failing when its call is cut short, so storing that
// would show a fabricated outage for a whole TTL. Both problems disappear by
// running the probe on a context that cannot be cancelled by a requester. It is
// still bounded — the backup client applies its own per-method budget — so this
// cannot leak a goroutine forever.
//
// # Waiters do not block on a refresh
//
// Once anything is cached, get returns it immediately and lets the refresh land
// in the background. A refresh that took longer than the scrape timeout would
// otherwise fail one scrape per TTL, and because the backup series are written
// after the process metrics, that scrape would lose the runtime gauges too.
//
// The single exception is a COLD cache, where get waits: the alternative is
// reporting an empty backup block, and "backup is enabled and replicating
// nothing" is a legitimate state (knomit creates no repo on its own), so an
// empty answer would be indistinguishable from an all-clear. That wait happens
// once per process and is bounded by the caller's own context.
type backupStatusCache struct {
	fetch func(context.Context) []BackupDBStatus
	ttl   time.Duration
	now   func() time.Time

	mu  sync.Mutex
	at  time.Time
	val []BackupDBStatus
	// valid is true once ANY probe has completed. It is what separates "nothing
	// has been asked yet" from "the answer is an empty list".
	valid bool
	// inflight is non-nil while a probe is running, and is closed when it
	// finishes. It is the single-flight: concurrent consumers share one probe
	// rather than each paying for a full remote listing per database.
	inflight chan struct{}
}

// get returns the cached status, starting a background refresh when it has
// expired and waiting only if nothing has ever been cached.
//
// The returned slice is shared with every other caller and must be treated as
// read-only.
func (c *backupStatusCache) get(ctx context.Context) []BackupDBStatus {
	c.mu.Lock()
	if c.valid && c.now().Sub(c.at) < c.ttl {
		val := c.val
		c.mu.Unlock()
		return val
	}
	c.startLocked(ctx)
	done, valid, val := c.inflight, c.valid, c.val
	c.mu.Unlock()

	if valid {
		// Stale but real. Serving it now keeps the scrape fast and honest about
		// its own age (the last_sync gauge carries the timestamps); blocking
		// here would put a two-minute round trip in the path of a ten-second
		// scrape once every TTL.
		return val
	}

	select {
	case <-done:
	case <-ctx.Done():
		// The caller gave up. The probe keeps running and will populate the
		// cache for whoever asks next — which is the entire point of detaching
		// it.
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

// startLocked begins a probe unless one is already running. Callers hold c.mu.
func (c *backupStatusCache) startLocked(ctx context.Context) {
	if c.inflight != nil {
		return
	}
	done := make(chan struct{})
	c.inflight = done
	// WithoutCancel, not the caller's context: see the type comment. Values
	// (trace ids and the like) are preserved; cancellation and deadlines are
	// not, because they belong to one requester and this answer belongs to all
	// of them.
	probeCtx := context.WithoutCancel(ctx)
	go func() {
		defer close(done)
		val := c.fetch(probeCtx)
		c.mu.Lock()
		c.val, c.at, c.valid, c.inflight = val, c.now(), true, nil
		c.mu.Unlock()
	}()
}

// backupStatus returns the current replication status, or nil when backup is
// disabled (no hook installed).
func (s *Server) backupStatus(ctx context.Context) []BackupDBStatus {
	if s.backup == nil {
		return nil
	}
	return s.backup.get(ctx)
}

// writeBackupMetrics appends the three replication series named by the backup
// design, in Prometheus text exposition format.
//
// What is DELIBERATELY not emitted matters as much as what is:
//
//   - No knomit_backup_txid_lag for a database whose status probe failed, or
//     that is paused. Neither has transaction ids, so the only value available
//     is 0 — and 0 on a lag gauge is precisely "the replica is caught up", the
//     opposite of what happened. An absent series is the honest answer, and
//     alerting rules can say absent() about it.
//   - No knomit_backup_last_sync_seconds for a database that has never synced.
//     Its zero is "never", not a timestamp; emitting it would report a
//     successful sync at the Unix epoch and make every staleness rule compute
//     an age of 56 years. Absent is again the answer alerts can act on.
//
// knomit_backup_error is emitted for EVERY database, including paused ones (as
// 0), so the series never vanishes while a database still exists. It is a gauge
// and is named like one: status is a point-in-time probe, so the only truthful
// value is "is this database erroring right now" (1 or 0). The design called it
// knomit_backup_errors_total, but a _total suffix on a non-counter is flagged by
// promtool and makes some dashboards apply rate() to a number that goes both
// ways — a HELP string mitigates that for humans and not for tooling.
func writeBackupMetrics(w io.Writer, sts []BackupDBStatus) {
	if len(sts) == 0 {
		return
	}
	sorted := make([]BackupDBStatus, len(sts))
	copy(sorted, sts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var lag, lastSync, errs []string
	for _, st := range sorted {
		lbl := fmt.Sprintf("{db=%s}", promLabelValue(st.Name))
		if st.LastError == "" && !st.Paused {
			lag = append(lag, fmt.Sprintf("knomit_backup_txid_lag%s %d\n",
				lbl, int64(st.LocalTXID)-int64(st.RemoteTXID)))
		}
		if st.LastSyncUnix != 0 {
			lastSync = append(lastSync, fmt.Sprintf("knomit_backup_last_sync_seconds%s %d\n", lbl, st.LastSyncUnix))
		}
		n := 0
		if st.LastError != "" {
			n = 1
		}
		errs = append(errs, fmt.Sprintf("knomit_backup_error%s %d\n", lbl, n))
	}

	writeFamily(w, "knomit_backup_txid_lag", "gauge",
		"Transactions the replica is behind the local database (local minus remote). "+
			"Absent for a database whose status could not be read or that is paused.", lag)
	writeFamily(w, "knomit_backup_last_sync_seconds", "gauge",
		"Unix time of this database's last successful replica sync. Absent when it has never synced. "+
			"NOTE: the source is in-memory in the backup agent, so an agent restart resets it to never "+
			"and this series DISAPPEARS until the first sync afterwards - an absent() staleness rule "+
			"will fire on every agent restart; alert on the value's age instead.", lastSync)
	writeFamily(w, "knomit_backup_error", "gauge",
		"1 while this database's last replication status probe reported an error, 0 otherwise. "+
			"A gauge: status is a point-in-time probe, not an accumulating count.", errs)
}

// writeFamily emits one metric family, and nothing at all when it has no
// series — a HELP/TYPE header with no samples under it says less than silence
// and trips exposition linters.
func writeFamily(w io.Writer, name, typ, help string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	for _, l := range lines {
		_, _ = io.WriteString(w, l)
	}
}

// promLabelValue renders a label value with the quoting the Prometheus text
// format requires: backslash, double quote and newline escaped.
func promLabelValue(v string) string {
	return `"` + promLabelEscaper.Replace(v) + `"`
}

var promLabelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
