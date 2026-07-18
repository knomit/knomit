package testenv

import (
	"context"

	"knomit/internal/store"
)

// MergeFrom merges the source branch into this branch using the given
// conflict strategy. Wraps store.BranchIndex.MergeBranch and captures the
// resulting HEAD as a Snapshot. Auto-verifies the repo unless
// StoryboardOpts.AutoVerify is false.
//
// The merge is a real git merge: fast-forward when the receiver is an
// ancestor of src, a merge commit with two parents for diverged histories,
// or a no-op when src is already reachable from the receiver or the
// strategy skips every conflicting change. See repoHandler.MergeBranch
// for the full semantics.
//
// Returns the Snapshot of the new HEAD on the receiver branch. If the
// merge is a no-op the Snapshot captures the unchanged HEAD.
//
// Note: the production MergeBranch generates its own merge commit
// message ("merge: src into dst (strategy)") — the DSL does not take a
// custom message argument. If tests need merge-message assertions,
// read via the commit object directly.
func (b *BranchHandle) MergeFrom(src *BranchHandle, strategy store.ConflictStrategy) *Snapshot {
	return b.mergeFrom(src, strategy, "")
}

// MergeFromAs is MergeFrom with an explicit snapshot name.
func (b *BranchHandle) MergeFromAs(name string, src *BranchHandle, strategy store.ConflictStrategy) *Snapshot {
	return b.mergeFrom(src, strategy, name)
}

func (b *BranchHandle) mergeFrom(src *BranchHandle, strategy store.ConflictStrategy, name string) *Snapshot {
	t := b.repo.sb.t
	t.Helper()
	if src == nil {
		t.Fatalf("MergeFrom: nil src on branch %s", b.name)
	}
	if src.repo != b.repo {
		t.Fatalf("MergeFrom: src branch %q belongs to a different repo than dst %q", src.name, b.name)
	}

	var mergeErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		mergeErr = svc.Branches().MergeBranch(context.Background(), src.name, b.name, strategy)
	})
	if mergeErr != nil {
		t.Fatalf("MergeFrom(%s -> %s, %s): %v", src.name, b.name, strategy, mergeErr)
	}

	// Resolve the new HEAD via the production API. The merge may be a
	// no-op (same hash, already-merged, or strategy-limited empty merge),
	// in which case HeadCommit still returns the pre-merge hash — that's
	// correct behavior, we just pin the current tip as the snapshot.
	var newHead string
	var headErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		newHead, headErr = svc.Branches().HeadCommit(context.Background(), b.name)
	})
	if headErr != nil {
		t.Fatalf("MergeFrom(%s -> %s): resolve new HEAD: %v", src.name, b.name, headErr)
	}

	snap := b.pushSnapshot(name, newHead)
	if b.repo.sb.auto {
		AssertIntegrity(t, b.repo)
	}
	return snap
}
