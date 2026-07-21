package testenv

import (
	"context"

	"knomit/internal/store"
)

// BatchWriter collects file writes for a single batched commit. Used inside
// the closure passed to BranchHandle.Batch. All writes staged via this
// writer land in one git commit with one commit message.
//
// Batched writes go through store.BatchWriteFacts directly, not the per-file
// Write path, so they match the real production batch-import code path.
type BatchWriter struct {
	files map[string]string
}

// Write stages a fact write inside a batch. The spec is serialized
// immediately via FactSpec.Build(), so later mutations to the spec do not
// affect what gets committed.
func (w *BatchWriter) Write(path string, spec FactSpec) {
	if w.files == nil {
		w.files = map[string]string{}
	}
	w.files[path] = spec.Build()
}

// Batch executes fn against a BatchWriter and commits all staged writes
// as a single commit via store.BatchWriteFacts. Returns a Snapshot pinning
// the batch commit. Auto-verifies the repo unless StoryboardOpts.AutoVerify
// is false.
//
// Example:
//
//	snap := agent.Batch("bulk import", func(w *testenv.BatchWriter) {
//	    w.Write("kb/a.md", testenv.Fact("a"))
//	    w.Write("kb/b.md", testenv.Fact("b"))
//	    w.Write("kb/c.md", testenv.Fact("c"))
//	})
//
// An empty batch (the closure stages zero writes) is a no-op: the function
// does not call BatchWriteFacts at all, returns nil, and does not push a
// snapshot. Tests that want to assert "batch with empty closure does not
// advance HEAD" can check the nil return.
func (b *BranchHandle) Batch(message string, fn func(w *BatchWriter)) *Snapshot {
	return b.batch("", message, fn)
}

// BatchAs is Batch with an explicit snapshot name.
func (b *BranchHandle) BatchAs(name, message string, fn func(w *BatchWriter)) *Snapshot {
	return b.batch(name, message, fn)
}

func (b *BranchHandle) batch(name, message string, fn func(w *BatchWriter)) *Snapshot {
	t := b.repo.sb.t
	t.Helper()
	w := &BatchWriter{}
	fn(w)
	if len(w.files) == 0 {
		return nil
	}
	var commit string
	var batchErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		commit, _, batchErr = svc.Facts().BatchWriteFacts(
			context.Background(), b.name, w.files, nil, message, "test")
	})
	if batchErr != nil {
		t.Fatalf("Batch(%s on %s): %v", message, b.name, batchErr)
	}
	snap := b.pushSnapshot(name, commit)
	if b.repo.sb.auto {
		AssertIntegrity(t, b.repo)
	}
	return snap
}
