//go:build contract

// Phase-1 contract cells for the origin-sync fault matrix. These assert the
// DESIRED graceful behavior of the product remote path under injected faults.
// Many are RED today — that is the point: `go test -tags contract ./internal/
// storytests/` produces the real bug inventory that Phase-2 fixes turn green.
// They are build-tagged out of the default suite so CI stays green until the
// product is fixed cell-by-cell.
package storytests

import (
	"testing"
	"time"

	"knomit/internal/testenv"
	"knomit/internal/testenv/gitserver"
)

// Cell: clone × N3 (connect-then-hang).
// CONTRACT: a clone against a remote that accepts the connection but never
// responds MUST abort within a bounded time — the product must not stall
// forever. Maps to reported symptom #1 ("cloning remote repos was stalling").
// RED TODAY: the product clone path (store.CloneFrom/InitFromRemote) calls
// go-git without any context/deadline, so this hangs until the budget below.
func TestContract_Clone_Stall_AbortsWithinDeadline(t *testing.T) {
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("a") // boot a clean manager (no origin yet) on the test goroutine
	remote := sb.BareRemoteHTTP("origin")

	// Server accepts the connection then never answers the info/refs
	// advertisement — the classic "connected but no bytes" stall.
	remote.Fault().SetHang(gitserver.ClassInfoRefs, true)

	// A clone that behaves well aborts well within this budget. If it does
	// not return at all, the product has no network timeout — the bug.
	const budget = 20 * time.Second

	done := make(chan error, 1)
	go func() { done <- repo.TryConnect(remote) }()

	select {
	case err := <-done:
		// Once a deadline is added (Phase 2) the clone returns an error
		// promptly instead of stalling.
		if err == nil {
			t.Fatal("expected clone against a hung remote to fail, got nil")
		}
		t.Logf("clone aborted as desired: %v", err)
	case <-time.After(budget):
		t.Fatalf("CONTRACT VIOLATION (symptom #1): clone did not abort within %s "+
			"against a connect-then-hang remote — the product clone path has no "+
			"network timeout/context deadline", budget)
	}
}
