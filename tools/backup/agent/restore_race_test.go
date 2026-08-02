package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"

	"knomit/internal/backup/proto"
)

// TestOverwritingRestoreRefusesADestinationTrackedDuringTheDownload pins the
// re-check inside restoreOverwriting's opMu section.
//
// The cheap check at the top of restoreOverwriting cannot stand alone. The
// download between it and the rename can legitimately run for minutes, Serve
// dispatches every request in its own goroutine, and track takes opMu — so a
// database can start replicating the destination while the restore is still
// fetching it. Without the second check the restore then clears that live
// database's sidecars and renames over the file the agent has open.
//
// The race is made deterministic rather than raced for. The test holds opMu
// itself, which is exactly where the restore must block after its download; it
// registers the destination while holding it, then releases. A restore that
// re-checks under opMu therefore ALWAYS sees the new entry and refuses, and one
// that does not take opMu at all runs straight through to a successful rename
// while the test is still holding the mutex — so the assertion below fails
// deterministically in that direction too, rather than flaking.
func TestOverwritingRestoreRefusesADestinationTrackedDuringTheDownload(t *testing.T) {
	a, home := newTestAgent(t)

	dst := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dst, "live")

	// Put something in the replica so the restore has real work to do and can
	// actually reach the rename.
	track(t, a, "core", dst, "repos/core.db", false)
	waitReplicatedPast(t, a, "core", 0)
	if err := a.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}

	// Hold the mutation lock. This is where the restore must wait.
	a.opMu.Lock()

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := a.restoreOverwriting(context.Background(), proto.RestoreParams{
			Rel: "repos/core.db", Dest: dst, Overwrite: true,
		})
		done <- result{ok, err}
	}()

	// Let the restore get past its early check and through the download. It
	// cannot pass opMu while this test holds it, so this sleep only decides how
	// much of the download happens before the injection — never the outcome.
	time.Sleep(300 * time.Millisecond)

	// A database starts replicating the destination, as a concurrent track
	// would. Written directly because Track itself takes opMu, which this test
	// is holding.
	a.mu.Lock()
	a.dbs["core"] = tracked{db: litestream.NewDB(dst)}
	a.mu.Unlock()

	a.opMu.Unlock()

	var got result
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("restoreOverwriting never returned")
	}

	if got.err == nil {
		t.Fatalf("restoreOverwriting overwrote a database that started replicating during the "+
			"download (restored = %v); the opMu re-check is what prevents this", got.ok)
	}
	if !strings.Contains(got.err.Error(), "replicating it right now") {
		t.Errorf("error = %v, want the tracked-destination refusal", got.err)
	}

	// And the live database is untouched.
	if got, want := readValue(t, dst), "live"; got != want {
		t.Errorf("destination value = %q, want %q; the restore wrote over a live database", got, want)
	}
}

// readValue reads the single seeded row back out of a database.
func readValue(t *testing.T, path string) string {
	t.Helper()
	db := openDB(t, path)
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil {
		t.Fatalf("query %s: %v", path, err)
	}
	return v
}
