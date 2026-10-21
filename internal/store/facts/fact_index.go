package facts

import (
	"context"
	"sync/atomic"

	"knomit/internal/fact"
	"knomit/internal/store/internal/vcs"
)

// ParseFactRecord parses a fact file into the stored record (no blob hash).
func ParseFactRecord(path, content string) (FactRecord, error) {
	f, err := fact.ParseFact(path, content)
	if err != nil {
		return FactRecord{}, err
	}
	return NewFactRecord(f, ""), nil
}

// Index owns all git-backed fact operations: reading and writing.
// All shared git-level plumbing lives on repoHandler — Index reaches up
// via fi.rh.*. In particular, index sync after a commit happens inside
// rh.NotifyCommit (which is called from WriteFile / deleteFile / batchWrite),
// so Index no longer holds its own IndexManager back-reference.
type Index struct {
	rh *Repo
	// deleteFault is the failure-injection hook of SetDeleteFaultForTest
	// (testseams.go); nil in production, where nothing can set it.
	deleteFault atomic.Pointer[func(path string) error]
}

// NewIndex builds the fact index over rh. The parent's constructors call it
// once per Service.
func NewIndex(rh *Repo) *Index { return &Index{rh: rh} }

// DiffFiles returns paths added/modified/deleted between fromCommit and the tip of branch.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (fi *Index) DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error) {
	return fi.rh.vr.DiffFiles(ctx, branch, fromCommit)
}

// ChangesUnder reports what differs under a folder between two commits. See
// changes.go: a read of two trees, no clock, no write.
func (fi *Index) ChangesUnder(ctx context.Context, branch string, q vcs.ChangesQuery) (vcs.ChangesResult, error) {
	return fi.rh.vr.ChangesUnder(ctx, branch, q)
}
