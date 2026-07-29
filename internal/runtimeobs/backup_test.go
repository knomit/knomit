package runtimeobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatusIncludesBackupBlock(t *testing.T) {
	s := NewServer(Options{
		BackupStatus: func(context.Context) []BackupDBStatus {
			return []BackupDBStatus{{Name: "core", LocalTXID: 7, RemoteTXID: 7, InSync: true}}
		},
	})
	rr := serve(t, s, "GET", "/runtime/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Backup []struct {
			Name       string `json:"name"`
			LocalTXID  uint64 `json:"local_txid"`
			RemoteTXID uint64 `json:"remote_txid"`
			InSync     bool   `json:"in_sync"`
		} `json:"backup"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rr.Body.String())
	}
	if len(body.Backup) != 1 || body.Backup[0].Name != "core" || !body.Backup[0].InSync {
		t.Errorf("backup block = %+v, want one in-sync core entry", body.Backup)
	}
}

func TestStatusOmitsBackupWhenDisabled(t *testing.T) {
	rr := serve(t, NewServer(Options{}), "GET", "/runtime/status")
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body["backup"]; present {
		t.Error("backup key present while backup is disabled; want omitted")
	}
}

// TestStatusRendersAnEmptyBackupBlockAsAnArray: backup enabled with nothing
// tracked yet is `[]`, never `null`. A consumer that ranges over the value must
// not have to tell the two apart, and `null` reads as "the hook is broken".
func TestStatusRendersAnEmptyBackupBlockAsAnArray(t *testing.T) {
	s := NewServer(Options{BackupStatus: func(context.Context) []BackupDBStatus { return nil }})
	rr := serve(t, s, "GET", "/runtime/status")
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, present := body["backup"]
	if !present {
		t.Fatal("backup key absent while the hook is installed; want an empty array")
	}
	if string(raw) != "[]" {
		t.Errorf("backup = %s, want []", raw)
	}
}

// TestStatusPreservesTheReconciliationError pins the meaning Manager.Status
// works to produce: a database knomit believes is replicated and the agent has
// never heard of is reported WITH its explanation, not collapsed into a blank
// entry that reads as "unknown but probably fine".
func TestStatusPreservesTheReconciliationError(t *testing.T) {
	const msg = "not registered with the replication agent: knomit believes this database is " +
		"being replicated and the agent does not, so it is NOT being backed up"
	s := NewServer(Options{
		BackupStatus: func(context.Context) []BackupDBStatus {
			return []BackupDBStatus{{Name: "core", LastError: msg}}
		},
	})
	rr := serve(t, s, "GET", "/runtime/status")
	var body struct {
		Backup []BackupDBStatus `json:"backup"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Backup) != 1 || body.Backup[0].LastError != msg {
		t.Fatalf("backup block = %+v, want the reconciliation error verbatim", body.Backup)
	}
}

// TestStatusMarksAPausedDatabase: a paused database is not tracked and not
// erroring, and rendering it as neither would make it vanish — the false
// all-clear this field exists to prevent.
func TestStatusMarksAPausedDatabase(t *testing.T) {
	s := NewServer(Options{
		BackupStatus: func(context.Context) []BackupDBStatus {
			return []BackupDBStatus{{Name: "core", Paused: true}}
		},
	})
	rr := serve(t, s, "GET", "/runtime/status")
	var body struct {
		Backup []BackupDBStatus `json:"backup"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Backup) != 1 || !body.Backup[0].Paused {
		t.Fatalf("backup block = %+v, want core marked paused", body.Backup)
	}
}

func TestMetricsEmitsTheThreeBackupSeries(t *testing.T) {
	synced := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	s := NewServer(Options{
		BackupStatus: func(context.Context) []BackupDBStatus {
			return []BackupDBStatus{
				{Name: "core", LocalTXID: 9, RemoteTXID: 7, LastSyncUnix: synced.Unix()},
				{Name: "archive/2a", LastError: "boom", LastSyncUnix: synced.Unix()},
			}
		},
	})
	body := serve(t, s, "GET", "/metrics").Body.String()

	for _, want := range []string{
		"# TYPE knomit_backup_txid_lag gauge\n",
		// local 9, remote 7: the replica is TWO transactions behind. The sign
		// says "how far behind the replica is", so a lagging replica is
		// positive.
		`knomit_backup_txid_lag{db="core"} 2` + "\n",
		"# TYPE knomit_backup_last_sync_seconds gauge\n",
		fmt.Sprintf("knomit_backup_last_sync_seconds{db=%q} %d\n", "core", synced.Unix()),
		`knomit_backup_error{db="core"} 0` + "\n",
		`knomit_backup_error{db="archive/2a"} 1` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q:\n%s", want, body)
		}
	}
	// A database whose status probe FAILED has no txids at all, so emitting a
	// lag of 0 for it would report the exact opposite of what happened.
	if strings.Contains(body, `knomit_backup_txid_lag{db="archive/2a"}`) {
		t.Errorf("/metrics emitted a lag for a database with no status; 0 reads as in-sync:\n%s", body)
	}
}

// TestMetricsOmitsLastSyncWhenNeverSynced: zero means "never synced". Rendering
// it verbatim would claim the database last synced at the Unix epoch, and every
// staleness alert would then be computed against 1970.
func TestMetricsOmitsLastSyncWhenNeverSynced(t *testing.T) {
	s := NewServer(Options{
		BackupStatus: func(context.Context) []BackupDBStatus {
			return []BackupDBStatus{{Name: "core", LocalTXID: 1, RemoteTXID: 1, InSync: true}}
		},
	})
	body := serve(t, s, "GET", "/metrics").Body.String()
	if strings.Contains(body, "knomit_backup_last_sync_seconds") {
		t.Errorf("/metrics reported a last-sync time for a database that has never synced:\n%s", body)
	}
	if !strings.Contains(body, `knomit_backup_txid_lag{db="core"} 0`) {
		t.Errorf("/metrics missing the lag gauge:\n%s", body)
	}
}

// TestMetricsKeepsAPausedDatabaseVisibleWithoutClaimingItIsInSync: a paused
// database has no transaction ids to compare, so a lag of 0 would say "caught
// up" about one that is not being replicated at all. It still gets an
// errors_total series, so the database does not vanish from the scrape while it
// exists — a series that disappears and a series that reads zero are both bad,
// and this picks the failure an operator can actually alert on.
func TestMetricsKeepsAPausedDatabaseVisibleWithoutClaimingItIsInSync(t *testing.T) {
	s := NewServer(Options{
		BackupStatus: func(context.Context) []BackupDBStatus {
			return []BackupDBStatus{{Name: "core", Paused: true}}
		},
	})
	body := serve(t, s, "GET", "/metrics").Body.String()
	if strings.Contains(body, `knomit_backup_txid_lag{db="core"}`) {
		t.Errorf("/metrics reported a lag for a paused database; 0 reads as in-sync:\n%s", body)
	}
	if !strings.Contains(body, `knomit_backup_error{db="core"} 0`) {
		t.Errorf("/metrics dropped the paused database entirely:\n%s", body)
	}
}

func TestMetricsOmitsBackupSeriesWhenDisabled(t *testing.T) {
	body := serve(t, NewServer(Options{}), "GET", "/metrics").Body.String()
	if strings.Contains(body, "knomit_backup") {
		t.Errorf("/metrics carries backup series while backup is disabled:\n%s", body)
	}
}

// TestBackupStatusIsCachedAcrossScrapes is the whole reason this hook is not
// wired straight through. Manager.Status costs one UNPAGINATED remote LTX
// listing per tracked database — every archived one included — and deliberately
// does not cache. A /metrics scrape and a /runtime/status poll on a timer would
// otherwise turn the operational surface into a load generator against the
// object store, scaling with the archive count.
func TestBackupStatusIsCachedAcrossScrapes(t *testing.T) {
	var calls atomic.Int64
	probed := make(chan struct{}, 8)
	now := time.Now()
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	s := NewServer(Options{
		BackupStatusTTL: 30 * time.Second,
		now:             clock,
		BackupStatus: func(context.Context) []BackupDBStatus {
			calls.Add(1)
			probed <- struct{}{}
			return []BackupDBStatus{{Name: "core", LocalTXID: 1, RemoteTXID: 1, InSync: true}}
		},
	})

	serve(t, s, "GET", "/metrics")
	serve(t, s, "GET", "/metrics")
	serve(t, s, "GET", "/runtime/status")
	if got := calls.Load(); got != 1 {
		t.Fatalf("backup status probed %d times inside the TTL, want 1", got)
	}

	clockMu.Lock()
	now = now.Add(31 * time.Second)
	clockMu.Unlock()
	<-probed // drain the cold probe
	serve(t, s, "GET", "/metrics")
	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatalf("no refresh was started after the TTL expired (calls = %d)", calls.Load())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("backup status probed %d times after the TTL expired, want 2", got)
	}
}

// TestBackupStatusCachesEvenWhenTheScraperGivesUp is the failure this cache was
// silently exhibiting: net/http cancels a request context when the client
// disconnects, which is what Prometheus does at scrape_timeout — and the status
// round trip is allowed two minutes. A probe run on the requester's context is
// therefore cancelled on exactly the deployment the cache exists for, so nothing
// is ever stored and every consumer re-probes on its own schedule.
//
// Two things are asserted, and both are load-bearing: the probe never sees the
// cancellation, and its result is cached for whoever asks next.
func TestBackupStatusCachesEvenWhenTheScraperGivesUp(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	var sawCancel atomic.Bool
	c := &backupStatusCache{
		ttl: time.Minute,
		now: time.Now,
		fetch: func(ctx context.Context) []BackupDBStatus {
			calls.Add(1)
			close(started)
			<-release
			if ctx.Err() != nil {
				sawCancel.Store(true)
			}
			return []BackupDBStatus{{Name: "core", InSync: true}}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	gave := make(chan []BackupDBStatus, 1)
	go func() { gave <- c.get(ctx) }()
	<-started
	cancel()

	select {
	case got := <-gave:
		if got != nil {
			t.Errorf("a cold get that gave up returned %+v, want nil (nothing has been probed yet)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("get did not return when its caller's context was cancelled")
	}

	close(release)
	got := c.get(context.Background())
	if sawCancel.Load() {
		t.Error("the probe observed the scraper's cancellation; it must run detached")
	}
	if len(got) != 1 || got[0].Name != "core" {
		t.Fatalf("second get = %+v, want the cached result of the first probe", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("probed %d times; the cancelled scrape's probe must still have populated the cache", n)
	}
}

// TestBackupStatusServesStaleWhileRefreshing: once anything is cached, no
// request waits for a refresh. A refresh slower than the scrape timeout would
// otherwise fail one scrape per TTL — and since the backup series are written
// after the process metrics, that scrape would lose the runtime gauges too.
func TestBackupStatusServesStaleWhileRefreshing(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int64
	now := time.Now()
	var clockMu sync.Mutex
	c := &backupStatusCache{
		ttl: time.Second,
		now: func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now },
		fetch: func(context.Context) []BackupDBStatus {
			n := calls.Add(1)
			if n > 1 {
				<-release // every refresh after the first hangs
			}
			return []BackupDBStatus{{Name: "core", LocalTXID: uint64(n)}}
		},
	}
	defer close(release)

	if got := c.get(context.Background()); len(got) != 1 {
		t.Fatalf("cold get = %+v, want one entry", got)
	}

	clockMu.Lock()
	now = now.Add(2 * time.Second)
	clockMu.Unlock()

	done := make(chan []BackupDBStatus, 1)
	go func() { done <- c.get(context.Background()) }()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].LocalTXID != 1 {
			t.Errorf("stale get = %+v, want the previous probe's result", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request blocked on a slow refresh; a warm cache must answer immediately")
	}
}

// TestBackupStatusCollapsesConcurrentProbes: the cache's promise is that the
// probe rate is bounded by the TTL however many consumers poll. Without
// single-flight, a burst of scrapes against a cold cache each starts its own
// full remote listing per database — the exact load this exists to prevent.
func TestBackupStatusCollapsesConcurrentProbes(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int64
	c := &backupStatusCache{
		ttl: time.Minute,
		now: time.Now,
		fetch: func(context.Context) []BackupDBStatus {
			calls.Add(1)
			<-release
			return []BackupDBStatus{{Name: "core"}}
		},
	}

	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() { defer wg.Done(); c.get(context.Background()) }()
	}
	// Let them all pile onto the cold cache before the probe can finish.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("25 concurrent scrapes caused %d probes, want 1", n)
	}
}

// TestBackupStatusCacheHasADefaultTTL: an operator who never configures one
// must still not get an uncapped probe rate. A zero TTL would make the cache a
// no-op and quietly restore the behaviour it exists to prevent.
func TestBackupStatusCacheHasADefaultTTL(t *testing.T) {
	var calls atomic.Int64
	s := NewServer(Options{
		BackupStatus: func(context.Context) []BackupDBStatus {
			calls.Add(1)
			return nil
		},
	})
	serve(t, s, "GET", "/metrics")
	serve(t, s, "GET", "/metrics")
	if got := calls.Load(); got != 1 {
		t.Fatalf("backup status probed %d times with no configured TTL, want 1", got)
	}
}
