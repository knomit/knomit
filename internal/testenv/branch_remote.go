package testenv

import (
	"context"

	"knomit/internal/store"
)

// Push pushes this branch to the repo's configured origin remote. Returns
// the store.PushResult so tests can inspect whether the push was forced, a
// no-op, etc. Fails the test on a Push error. Auto-verifies the repo after
// the push.
//
// The repo must be Connect()ed to a RemoteHandle first. If no origin is
// configured, the production Push returns PushResult{} with no error
// (treating it as a no-op) — this matches the normal "no remote" test case.
func (b *BranchHandle) Push() store.PushResult {
	t := b.repo.sb.t
	t.Helper()
	var result store.PushResult
	var pushErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		result, pushErr = svc.Remote().Push(context.Background(), b.name, nil)
	})
	if pushErr != nil {
		t.Fatalf("Push(%s): %v", b.name, pushErr)
	}
	if b.repo.sb.auto {
		AssertIntegrity(t, b.repo)
	}
	return result
}

// Sync runs one reconcile cycle (fetch + reconcileMain + reconcileAgent)
// using the production Sync code path. Returns the store.SyncResult so
// tests can inspect Main / Agent outcomes. Fails the test on a Sync error.
// Auto-verifies the repo after the sync.
//
// After Sync, the branch's snapshot stack captures the new HEAD via
// pushSnapshot so subsequent At/AtIndex/AtName work consistently with
// other mutation methods.
func (b *BranchHandle) Sync() store.SyncResult {
	t := b.repo.sb.t
	t.Helper()
	var result store.SyncResult
	var syncErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		result, syncErr = svc.Remote().Sync(context.Background(), b.name, nil)
	})
	if syncErr != nil {
		t.Fatalf("Sync(%s): %v", b.name, syncErr)
	}
	// Capture the resulting HEAD as a snapshot if the agent branch advanced.
	if result.Agent.Mode == store.ModeRebase || result.Agent.Mode == store.ModeFF || result.Agent.Mode == store.ModeMerge {
		var headHash string
		var headErr error
		b.repo.ri.WithRead(func(svc *store.Service) {
			headHash, headErr = svc.Branches().HeadCommit(context.Background(), b.name)
		})
		if headErr != nil {
			t.Fatalf("Sync(%s): resolve new HEAD: %v", b.name, headErr)
		}
		b.pushSnapshot("", headHash)
	}
	if b.repo.sb.auto {
		AssertIntegrity(t, b.repo)
	}
	return result
}
