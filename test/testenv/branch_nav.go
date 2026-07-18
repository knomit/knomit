package testenv

import (
	"context"

	"knomit/internal/store"
)

// Head returns a Snapshot for the current HEAD of the branch. If the
// branch has captured snapshots via prior mutations, returns the most
// recent one. Otherwise resolves the branch's git HEAD via the production
// API and returns a synthetic snapshot named "HEAD".
func (b *BranchHandle) Head() *Snapshot {
	if n := len(b.snapshots); n > 0 {
		return b.snapshots[n-1]
	}
	t := b.repo.sb.t
	t.Helper()
	var hash string
	var err error
	b.repo.ri.WithRead(func(svc *store.Service) {
		hash, err = svc.Branches().HeadCommit(context.Background(), b.name)
	})
	if err != nil {
		t.Fatalf("Head(%s): %v", b.name, err)
	}
	return &Snapshot{Name: "HEAD", Commit: hash, Branch: b}
}

// At returns the given snapshot pointer (sanity-checks it belongs to
// this branch). Provided for symmetry with AtIndex and AtName — when
// the test already has a Snapshot from a mutation return value, At is
// the documentation-friendly way to say "read at this snapshot."
func (b *BranchHandle) At(s *Snapshot) *Snapshot {
	t := b.repo.sb.t
	t.Helper()
	if s == nil {
		t.Fatalf("At: nil snapshot on branch %s", b.name)
	}
	if s.Branch != b {
		t.Fatalf("At: snapshot %q belongs to branch %q, not %q", s.Name, s.Branch.name, b.name)
	}
	return s
}

// AtIndex returns a snapshot by relative index. Negative indices count
// from the most recent snapshot: -1 is the latest mutation, -2 is the
// previous, etc. Non-negative indices count from the start of the stack
// (0 is the oldest snapshot on this handle, 1 the next, etc).
func (b *BranchHandle) AtIndex(i int) *Snapshot {
	t := b.repo.sb.t
	t.Helper()
	n := len(b.snapshots)
	if n == 0 {
		t.Fatalf("AtIndex(%d): no snapshots captured on branch %s", i, b.name)
	}
	idx := i
	if i < 0 {
		idx = n + i
	}
	if idx < 0 || idx >= n {
		t.Fatalf("AtIndex(%d): out of range [%d, %d) on branch %s", i, -n, n, b.name)
	}
	return b.snapshots[idx]
}

// AtName looks up a snapshot by its assigned name. Works for both
// auto-generated names ("C1", "C2", ...) and explicit names set via
// WriteAs / UpdateAs / DeleteAs / BatchAs.
func (b *BranchHandle) AtName(name string) *Snapshot {
	t := b.repo.sb.t
	t.Helper()
	for _, s := range b.snapshots {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("AtName(%q): no such snapshot on branch %s (have: %v)", name, b.name, b.snapshotNames())
	return nil
}

func (b *BranchHandle) snapshotNames() []string {
	out := make([]string, len(b.snapshots))
	for i, s := range b.snapshots {
		out[i] = s.Name
	}
	return out
}
