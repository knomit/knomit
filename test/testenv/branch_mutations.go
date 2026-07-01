package testenv

import (
	"context"
	"strconv"

	"knomit/internal/store"
)

// Write creates or overwrites a fact at path with spec's content and
// auto-commits with the given message. Returns a Snapshot pinning the
// resulting commit. Auto-verifies the repo unless StoryboardOpts.AutoVerify
// is false.
//
// Each call produces exactly one commit. For multi-file commits, use
// BranchHandle.Batch (Task 2.5).
func (b *BranchHandle) Write(path string, spec FactSpec, message string) *Snapshot {
	return b.write(path, spec, message, "")
}

// WriteAs is Write with an explicit snapshot name (used by BranchHandle.AtName
// in Task 2.7). Useful when the test asserts against snapshots by name rather
// than by captured return value.
func (b *BranchHandle) WriteAs(name, path string, spec FactSpec, message string) *Snapshot {
	return b.write(path, spec, message, name)
}

// Update is an alias for Write that reads more naturally when the path
// already exists. Functionally identical to Write.
func (b *BranchHandle) Update(path string, spec FactSpec, message string) *Snapshot {
	return b.write(path, spec, message, "")
}

// UpdateAs is Update with an explicit snapshot name.
func (b *BranchHandle) UpdateAs(name, path string, spec FactSpec, message string) *Snapshot {
	return b.write(path, spec, message, name)
}

// write is the shared implementation for all Write/Update/*As variants.
// Uses repos.RepoInstance.WithRead to access the underlying store.Service
// under the repo's read lock — this is the only public way to reach the
// Facts() subservice without reaching into unexported fields.
func (b *BranchHandle) write(path string, spec FactSpec, message, name string) *Snapshot {
	t := b.repo.sb.t
	t.Helper()
	content := spec.Build()
	var res store.WriteFactResult
	var writeErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		res, writeErr = svc.Facts().WriteFact(
			context.Background(), b.name, path, content, message, "test")
	})
	if writeErr != nil {
		t.Fatalf("Write(%s on %s): %v", path, b.name, writeErr)
	}
	snap := b.pushSnapshot(name, res.CommitHash)
	if b.repo.sb.auto {
		AssertIntegrity(t, b.repo)
	}
	return snap
}

// Delete removes the fact at path on this branch and returns a Snapshot
// pinning the deletion commit. Auto-verifies unless disabled.
func (b *BranchHandle) Delete(path, message string) *Snapshot {
	return b.delete(path, message, "")
}

// DeleteAs is Delete with an explicit snapshot name.
func (b *BranchHandle) DeleteAs(name, path, message string) *Snapshot {
	return b.delete(path, message, name)
}

func (b *BranchHandle) delete(path, message, name string) *Snapshot {
	t := b.repo.sb.t
	t.Helper()
	var commit string
	var delErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		commit, delErr = svc.Facts().DeleteFact(
			context.Background(), b.name, path, message)
	})
	if delErr != nil {
		t.Fatalf("Delete(%s on %s): %v", path, b.name, delErr)
	}
	snap := b.pushSnapshot(name, commit)
	if b.repo.sb.auto {
		AssertIntegrity(t, b.repo)
	}
	return snap
}

// pushSnapshot appends a new snapshot to the branch's stack. If name is
// empty, auto-generates "C<N>" where N is the 1-based position.
func (b *BranchHandle) pushSnapshot(name, commit string) *Snapshot {
	if name == "" {
		name = "C" + strconv.Itoa(len(b.snapshots)+1)
	}
	snap := &Snapshot{Name: name, Commit: commit, Branch: b}
	b.snapshots = append(b.snapshots, snap)
	return snap
}
