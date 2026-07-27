package repos

import (
	"context"
	"errors"
	"fmt"

	"knomit/internal/store"
)

var (
	// ErrRepoDescriptionTooLong is returned by WriteKBManifest when content
	// exceeds MaxRepoDescriptionBytes. Mirrors ErrLensDescriptionTooLong: the
	// cap is a pure input check, enforced here rather than at the HTTP edge so
	// every writer of kb.md is bound by it.
	ErrRepoDescriptionTooLong = errors.New("repo description too long")
	// ErrAgentBranchUnset is returned when the repo has no agent branch yet, so
	// there is no ref to read the manifest from or commit it to.
	ErrAgentBranchUnset = errors.New("repo has no agent branch")
)

// MaxRepoDescriptionBytes caps the kb.md root manifest. Byte length, not rune
// count: it bounds stored + wire size. Deliberately far larger than
// MaxLensDescriptionBytes — a lens description is a one-line note about a read
// union, whereas kb.md is a repo's root manifest and routinely runs to several
// pages of guidance for the agents reading it.
const MaxRepoDescriptionBytes = 64 * 1024

// KBManifestPath is the repo's root manifest, at the root of the tree rather
// than under the ontology root: it is not a fact. The search indexer skips it
// as unparseable and the commits list filters to ontologyRoot, so writing it
// moves the file and its commit-log row, never the fact index or the history UI.
const KBManifestPath = "kb.md"

// kbManifestCommitMsg is the commit subject for every manifest edit, so the
// git log reads uniformly regardless of which client made the change.
const kbManifestCommitMsg = "docs: update kb.md root manifest"

// ReadKBManifest returns the verbatim content of kb.md at the tip of the repo's
// agent branch. A missing manifest is not an error — it returns "" with a nil
// error, because "this repo has no description" is an ordinary state.
func (ri *RepoInstance) ReadKBManifest(ctx context.Context) (string, error) {
	branch := ri.agentBranch
	if branch == "" {
		return "", ErrAgentBranchUnset
	}
	var content string
	// WithRead's contract: fn does not run unless a live service is available,
	// and the error says why — so there is no nil svc to guard against here.
	err := ri.WithRead(func(svc *store.Service) {
		res, rerr := svc.Facts().ReadFact(ctx, branch, KBManifestPath, nil)
		if rerr != nil {
			return // absent (or unreadable) — no description to report
		}
		content = res.Content
	})
	return content, err
}

// WriteKBManifest commits content to kb.md on the repo's agent branch — the
// exact file and branch ReadKBManifest reads, so an edit round-trips. It
// reports whether a commit was made: a byte-identical manifest is skipped,
// because the store's write path always builds a fresh commit object and
// re-saving unchanged text would otherwise append an empty commit to the agent
// branch (and push it to the remote).
//
// The read-compare-write is not atomic. That is the same last-write-wins
// contract the fact-write path already has: a racing writer can turn this call
// into a no-op, and the loser's content is what a subsequent read returns.
func (ri *RepoInstance) WriteKBManifest(ctx context.Context, content string) (committed bool, err error) {
	if len(content) > MaxRepoDescriptionBytes {
		return false, fmt.Errorf("%w: %d bytes exceeds the maximum of %d",
			ErrRepoDescriptionTooLong, len(content), MaxRepoDescriptionBytes)
	}
	branch := ri.agentBranch
	if branch == "" {
		return false, ErrAgentBranchUnset
	}
	var writeErr error
	// Capture WithRead's own error separately: it reports a closed or detached
	// store, in which case fn never ran at all. Dropping it would turn "the
	// write never happened" into a silent success.
	acquireErr := ri.WithRead(func(svc *store.Service) {
		if cur, rerr := svc.Facts().ReadFact(ctx, branch, KBManifestPath, nil); rerr == nil && cur.Content == content {
			return
		}
		if _, werr := svc.Facts().WriteFact(ctx, branch, KBManifestPath,
			content, kbManifestCommitMsg, "update"); werr != nil {
			writeErr = werr
			return
		}
		committed = true
	})
	if acquireErr != nil {
		return false, acquireErr
	}
	return committed, writeErr
}
