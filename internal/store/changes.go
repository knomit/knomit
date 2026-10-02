// Changes: what is different under a folder between an earlier commit and now.
//
// This is a read of TWO TREES and nothing else. It compares the tree at
// `since` with the tree at `head` under one folder and reports each fact path
// as added, modified or deleted. It reads no timestamp, no commit_log row and
// no index row, and it writes nothing: the caller keeps `head` and passes it
// back as its next `since`.
//
// Why not the commit log: commit time is set by whoever makes the commit, the
// commit_log tiebreak is this machine's own SQLite rowid, and a fast-forward
// over an agent's merge can put an older deleting commit behind the caller's
// bookmark, so a time-ordered replay can differ between machines and can lose
// a deletion. Two trees have none of those problems: the answer is a function
// of (tree(since), tree(head), prefix) only, so two machines holding the same
// two commits give byte-identical answers.
//
// The answer is the NET difference. A fact added and removed between two reads
// is not reported — for a reader of state (a task pool, an inbox) that is the
// right answer, because the thing is no longer there. Per-event history is the
// commit list's job, not this one's.
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
)

// Sentinel errors a changes read can return. Each one is a CLIENT error with a
// named remedy: none of them may ever be turned into an empty answer, because
// an empty answer reads as "nothing changed" and would be believed.
var (
	// ErrUnknownSince: `since` is not a full commit hash, or not a commit this
	// repo holds (another repo's, or one not fetched yet).
	ErrUnknownSince = errors.New("since is not a commit in this repo; omit since to re-list the folder as it is now")
	// ErrSinceNotBehind: `since` is a commit here but is not behind head
	// (neither head itself nor one of its ancestors). A reversed or sideways
	// diff would report paths as DELETED that nobody removed — in a task pool,
	// a fabricated take.
	ErrSinceNotBehind = errors.New("since is not behind head; retry after sync, or omit since to re-list the folder as it is now")
	// ErrInvalidPrefix: the prefix leaves its folder (`..`) or names private
	// state (a segment beginning with ".").
	ErrInvalidPrefix = errors.New("invalid prefix")
	// ErrInvalidChangesCursor: a paging cursor whose pinned head is not a
	// commit on this branch's history.
	ErrInvalidChangesCursor = errors.New("invalid changes cursor; restart without a cursor")
)

// Change values on a PathChange.
const (
	ChangeAdded    = "added"
	ChangeModified = "modified"
	ChangeDeleted  = "deleted"
)

// PathChange is one fact path that differs between since and head.
type PathChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// ChangesQuery is one page request.
type ChangesQuery struct {
	// Since is the caller's bookmark: a full commit hash. Empty means "from
	// nothing", so every fact under the prefix comes back as added.
	Since string
	// Head pins the newer side. Empty means "the branch tip now"; a paging
	// cursor sets it so every page of one read compares the SAME two trees.
	Head string
	// Prefix is the folder, relative to the ontology root. See
	// NormalizeChangesPrefix for the rules.
	Prefix string
	// After is the last path of the previous page; rows are strictly greater
	// (bytewise).
	After string
	// Limit is the page size; <= 0 means unlimited.
	Limit int
}

// ChangesResult is one page. Head is always set, including on an empty page.
type ChangesResult struct {
	Head    string
	Changes []PathChange
	HasMore bool
}

// NormalizeChangesPrefix turns a caller's folder into the repo path it names.
//
// The prefix is RELATIVE TO THE ONTOLOGY ROOT (`tasks/a` means
// `<ontologyRoot>/tasks/a`), lowercased because knomit writes lowercase fact
// paths, stripped of leading and trailing "/", and never given a ".md"
// suffix. It is refused when it contains ".." or any segment beginning with
// "." — private state is never a folder a changes read may walk.
//
// It deliberately does NOT use fact.NormalizePath: that function appends
// ".md", so `tasks/a` would become the single file `kb/tasks/a.md` and every
// read would silently come back empty.
func NormalizeChangesPrefix(ontologyRoot, prefix string) (string, error) {
	p := strings.Trim(strings.ToLower(prefix), "/")
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("%w: %q contains \"..\"", ErrInvalidPrefix, prefix)
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("%w: %q has an empty path segment", ErrInvalidPrefix, prefix)
	}
	if fact.IsPrivatePath(p) {
		return "", fmt.Errorf("%w: %q names private state (a segment beginning with \".\")", ErrInvalidPrefix, prefix)
	}
	root := strings.Trim(ontologyRoot, "/")
	switch {
	case root == "":
		return p, nil
	case p == "":
		return root, nil
	default:
		return root + "/" + p, nil
	}
}

// ChangesUnder reports the fact paths under prefix that differ between the
// tree at q.Since and the tree at head (q.Head, or the tip of branch).
//
// Rules, each of which a test pins:
//   - since must be head or an ancestor of head, else ErrSinceNotBehind;
//   - a folder missing at either end is the EMPTY tree (a lane's first post is
//     "added", its last removal is "deleted"), never an error;
//   - only .md paths, and never a path with a private segment below the prefix;
//   - rows sorted bytewise by path; a rename is "deleted" + "added" (no rename
//     detection).
func (rh *repoHandler) ChangesUnder(ctx context.Context, branch string, q ChangesQuery) (ChangesResult, error) {
	base, err := NormalizeChangesPrefix(rh.ontologyRoot(), q.Prefix)
	if err != nil {
		return ChangesResult{}, err
	}

	tip, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return ChangesResult{}, err
	}
	tipCommit, err := rh.repo.CommitObject(tip)
	if err != nil {
		return ChangesResult{}, fmt.Errorf("ChangesUnder: tip commit: %w", err)
	}

	headCommit := tipCommit
	if q.Head != "" {
		// A pinned head comes from a paging cursor. It must be a commit on
		// this branch's history — otherwise a cursor minted on one branch
		// could replay another branch's tree here.
		h, ok := parseFullHash(q.Head)
		if !ok {
			return ChangesResult{}, ErrInvalidChangesCursor
		}
		c, err := rh.repo.CommitObject(h)
		if err != nil {
			return ChangesResult{}, ErrInvalidChangesCursor
		}
		if c.Hash != tipCommit.Hash {
			behind, err := c.IsAncestor(tipCommit)
			if err != nil {
				return ChangesResult{}, fmt.Errorf("ChangesUnder: cursor ancestry: %w", err)
			}
			if !behind {
				return ChangesResult{}, ErrInvalidChangesCursor
			}
		}
		headCommit = c
	}

	var fromTree *object.Tree
	if q.Since != "" {
		h, ok := parseFullHash(q.Since)
		if !ok {
			return ChangesResult{}, ErrUnknownSince
		}
		sinceCommit, err := rh.repo.CommitObject(h)
		if err != nil {
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				return ChangesResult{}, ErrUnknownSince
			}
			return ChangesResult{}, fmt.Errorf("ChangesUnder: since commit: %w", err)
		}
		if sinceCommit.Hash != headCommit.Hash {
			behind, err := sinceCommit.IsAncestor(headCommit)
			if err != nil {
				return ChangesResult{}, fmt.Errorf("ChangesUnder: since ancestry: %w", err)
			}
			if !behind {
				return ChangesResult{}, fmt.Errorf("%w (branch %q)", ErrSinceNotBehind, branch)
			}
		}
		if fromTree, err = subtreeOrEmpty(sinceCommit, base); err != nil {
			return ChangesResult{}, err
		}
	}
	toTree, err := subtreeOrEmpty(headCommit, base)
	if err != nil {
		return ChangesResult{}, err
	}

	rows, err := diffSubtrees(fromTree, toTree, base, rh.isFactPath)
	if err != nil {
		return ChangesResult{}, err
	}

	res := ChangesResult{Head: headCommit.Hash.String(), Changes: []PathChange{}}
	for _, r := range rows {
		if q.After != "" && r.Path <= q.After {
			continue
		}
		if q.Limit > 0 && len(res.Changes) == q.Limit {
			res.HasMore = true
			break
		}
		res.Changes = append(res.Changes, r)
	}
	return res, nil
}

// DiffFacts is the trigger dispatcher's read: every fact path under the
// ontology root that differs between the tree at `from` and the tree at `to`,
// as added/modified/deleted, sorted bytewise. The same two-tree read as
// ChangesUnder — subtreeOrEmpty, diffSubtrees, isFactPath, no clock, no
// commit log, no write — with ONE difference: there is NO ancestry check. The
// dispatcher's advance is "old head → current head" of the agent branch, and
// after an origin rewind the old head is NOT an ancestor of the new one (the
// agent's commits were replayed under new hashes); the two trees are still
// meaningful, and refusing them would silently lose the fires. ChangesUnder
// keeps its check: its `since` is a client's bookmark, where a sideways diff
// fabricates deletions. A ZeroHash `from` is the empty tree.
func (rh *repoHandler) DiffFacts(ctx context.Context, from, to plumbing.Hash) ([]PathChange, error) {
	base := rh.ontologyRoot()
	var fromTree *object.Tree
	if from != plumbing.ZeroHash {
		fc, err := rh.repo.CommitObject(from)
		if err != nil {
			return nil, fmt.Errorf("DiffFacts: from commit %s: %w", from, err)
		}
		if fromTree, err = subtreeOrEmpty(fc, base); err != nil {
			return nil, err
		}
	}
	tc, err := rh.repo.CommitObject(to)
	if err != nil {
		return nil, fmt.Errorf("DiffFacts: to commit %s: %w", to, err)
	}
	toTree, err := subtreeOrEmpty(tc, base)
	if err != nil {
		return nil, err
	}
	return diffSubtrees(fromTree, toTree, base, rh.isFactPath)
}

// subtreeOrEmpty returns the tree at base in c, or nil (the empty tree) when
// that folder does not exist there. A missing folder is the normal lifecycle
// of a queue folder — it does not exist before the first post, and git drops
// it when the last entry is removed — so it must read as empty, never as an
// error and never as "no change".
func subtreeOrEmpty(c *object.Commit, base string) (*object.Tree, error) {
	root, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("ChangesUnder: tree of %s: %w", c.Hash, err)
	}
	if base == "" {
		return root, nil
	}
	sub, err := root.Tree(base)
	if errors.Is(err, object.ErrDirectoryNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ChangesUnder: subtree %q of %s: %w", base, c.Hash, err)
	}
	return sub, nil
}

// diffSubtrees diffs two folder trees (nil = empty) and returns repo-relative
// paths sorted bytewise, keeping only those isFact admits. The caller passes
// repoHandler.isFactPath — the indexer's own membership rule (under the
// ontology root, .md, not private) — so changes and the index agree on what a
// fact is, with one copy of the rule.
func diffSubtrees(from, to *object.Tree, base string, isFact func(string) bool) ([]PathChange, error) {
	if from == nil && to == nil {
		return nil, nil
	}
	changes, err := object.DiffTree(from, to)
	if err != nil {
		return nil, fmt.Errorf("ChangesUnder: diff: %w", err)
	}
	rows := make([]PathChange, 0, len(changes))
	for _, ch := range changes {
		var rel, kind string
		switch {
		case ch.From.Name == "" && ch.To.Name != "":
			rel, kind = ch.To.Name, ChangeAdded
		case ch.From.Name != "" && ch.To.Name == "":
			rel, kind = ch.From.Name, ChangeDeleted
		default:
			rel, kind = ch.To.Name, ChangeModified
		}
		p := rel
		if base != "" {
			p = base + "/" + rel
		}
		if !isFact(p) {
			continue
		}
		rows = append(rows, PathChange{Path: p, Change: kind})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows, nil
}

// parseFullHash accepts only a full 40-hex commit hash. plumbing.NewHash
// silently maps garbage to a zero-padded hash, which would then surface as a
// confusing "object not found" instead of a clear "unknown since".
func parseFullHash(s string) (plumbing.Hash, bool) {
	if len(s) != 40 || !isHex(s) {
		return plumbing.ZeroHash, false
	}
	return plumbing.NewHash(strings.ToLower(s)), true
}

// isHex reports whether s is all hex digits, in either case.
func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// ChangesScope is WHICH history a paged read compares: the repo (its 12-hex
// wire id) and the branch name. A cursor records it so every page reads the
// same repo and branch as the first. A cursor always carries both.
type ChangesScope struct {
	Repo   string
	Branch string
}

// changesCursor is the whole state of one paged read. It is stateless on the
// server by design — nothing is stored, nothing expires — because the answer
// is a pure function of (repo, branch, since, head, prefix). The MCP tool and
// the REST route share it, so a cursor minted by one is accepted by the other
// when both read the same repo and branch.
type changesCursor struct {
	Repo   string `json:"r,omitempty"`
	Branch string `json:"b,omitempty"`
	Since  string `json:"s,omitempty"`
	Head   string `json:"h"`
	Prefix string `json:"p,omitempty"`
	After  string `json:"a"`
}

// EncodeChangesCursor returns the opaque token for the page after `after`,
// which is the repo-relative path of the last row (never a kb:// wire path).
func EncodeChangesCursor(scope ChangesScope, since, head, prefix, after string) string {
	b, _ := json.Marshal(changesCursor{
		Repo: scope.Repo, Branch: scope.Branch,
		Since: since, Head: head, Prefix: prefix, After: after,
	})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeChangesCursor reverses EncodeChangesCursor. The returned query has no
// Limit; the caller sets one. A cursor must carry BOTH repo and branch; one
// missing either was never minted and is refused.
func DecodeChangesCursor(s string) (ChangesQuery, ChangesScope, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ChangesQuery{}, ChangesScope{}, ErrInvalidChangesCursor
	}
	var c changesCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Head == "" || c.After == "" || c.Repo == "" || c.Branch == "" {
		return ChangesQuery{}, ChangesScope{}, ErrInvalidChangesCursor
	}
	return ChangesQuery{Since: c.Since, Head: c.Head, Prefix: c.Prefix, After: c.After},
		ChangesScope{Repo: c.Repo, Branch: c.Branch}, nil
}

// ChangesBookmark is the bookmark a changes read hands back:
// "<repo12>:<commit40>". It names the repo the commit belongs to, so a caller
// replaying it against another repo is refused by name instead of being told
// "not a commit in this repo". It deliberately carries no branch: a bookmark
// taken on main and replayed on an experiment forked from it is a fair
// question, and the ancestry check refuses the ones that are not.
func ChangesBookmark(repo12, head string) string { return repo12 + ":" + head }

// ParseChangesSince splits a caller's `since` into the repo it names and the
// commit. A raw commit (no ":") names no repo — it means the repo being read —
// and comes back with repo "". A bookmark's repo half must be 12-hex and its
// commit half a full 40-hex hash, else ErrUnknownSince (an empty half must
// never read as "omit since"). Whether the commit exists is ChangesUnder's check.
func ParseChangesSince(since string) (repo, commit string, err error) {
	r, c, found := strings.Cut(since, ":")
	if !found {
		return "", since, nil
	}
	if len(r) != 12 || !isHex(r) {
		return "", "", fmt.Errorf("%w (since %q is neither a bookmark <repo12>:<commit40> nor a 40-hex commit)", ErrUnknownSince, since)
	}
	if _, ok := parseFullHash(c); !ok {
		return "", "", fmt.Errorf("%w (since %q is neither a bookmark <repo12>:<commit40> nor a 40-hex commit)", ErrUnknownSince, since)
	}
	return strings.ToLower(r), c, nil
}
