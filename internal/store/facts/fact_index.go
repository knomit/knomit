package facts

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing/transport"

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
	rh   *Repo
	auth transport.AuthMethod
}

// NewIndex builds the fact index over rh. The parent's constructors call it
// once per Service, and set the transport auth after open with SetAuth.
func NewIndex(rh *Repo) *Index { return &Index{rh: rh} }

// SetAuth sets the transport credentials the index hands to remote operations.
func (fi *Index) SetAuth(auth transport.AuthMethod) { fi.auth = auth }

// DiffFiles returns paths added/modified/deleted between fromCommit and the tip of branch.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (fi *Index) DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error) {
	return fi.rh.DiffFiles(ctx, branch, fromCommit)
}

// ChangesUnder reports what differs under a folder between two commits. See
// changes.go: a read of two trees, no clock, no write.
func (fi *Index) ChangesUnder(ctx context.Context, branch string, q vcs.ChangesQuery) (vcs.ChangesResult, error) {
	return fi.rh.ChangesUnder(ctx, branch, q)
}
