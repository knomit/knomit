//go:build contract

package storytests

import (
	"testing"
	"time"

	"knomit/test/testenv"
	"knomit/test/testenv/gitserver"
)

// Cell A — symptom #2: re-pointing origin to a hung remote must not block forever.
//
// Scenario: a repo is connected to a HEALTHY HTTP remote and settles, then the
// origin is RE-POINTED to a SECOND remote whose info/refs advertisement hangs
// (accepts the connection, never answers). The re-point drives the production
// PUT /api/v1/{repo}/origin flow (Origins.Set + ActivateSync's synchronous
// reconcile). ActivateSync fetches from the new origin; if that fetch is not
// bounded, the re-point blocks indefinitely.
//
// CONTRACT: the re-point aborts within a bounded time (well under the budget
// below), governed by cfg.Git.NetworkTimeout (5s in the Storyboard). It need
// not succeed — a hung remote SHOULD fail — but it must RETURN.
//
// Characterization: with the network-timeout fix threaded into ActivateSync's
// reconcile (fetchOrigin honours ri.rh.netTimeout), this is expected GREEN.
func TestContract_Repoint_HungRemote_AbortsWithinDeadline(t *testing.T) {
	sb := testenv.NewStoryboard(t)

	// 1. Connect to a healthy HTTP remote and let it settle.
	healthy := sb.BareRemoteHTTP("origin")
	healthy.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed on healthy origin")
	repo := sb.Repo("a").Connect(healthy)

	// 2. Build a SECOND remote whose info/refs hangs.
	hung := sb.BareRemoteHTTP("hung")
	hung.WriteMain("kb/other.md", testenv.Fact("other"), "content on hung origin")
	hung.Fault().SetHang(gitserver.ClassInfoRefs, true)

	// A well-behaved re-point aborts well within this budget. If it never
	// returns, the re-point path has no network deadline — the bug.
	const budget = 30 * time.Second

	done := make(chan error, 1)
	go func() { done <- repo.TryReConnect(hung) }()

	start := time.Now()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		// A hung remote SHOULD make the re-point fail; the point of the
		// contract is that it fails PROMPTLY rather than hanging.
		if err == nil {
			t.Fatalf("expected re-point to a hung remote to fail, got nil after %s", elapsed)
		}
		t.Logf("re-point aborted as desired after %s: %v", elapsed, err)
	case <-time.After(budget):
		t.Fatalf("CONTRACT VIOLATION (symptom #2): re-pointing origin to a hung "+
			"remote did not abort within %s — the re-point path (Origins.Set + "+
			"ActivateSync reconcile) has no network timeout/context deadline", budget)
	}
}
