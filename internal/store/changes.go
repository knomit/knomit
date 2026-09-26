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

	rows, err := diffSubtrees(fromTree, toTree, base)
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
// fact paths, sorted bytewise, with private paths and non-.md files dropped.
func diffSubtrees(from, to *object.Tree, base string) ([]PathChange, error) {
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
		if !strings.HasSuffix(rel, ".md") || fact.IsPrivatePath(rel) {
			continue
		}
		p := rel
		if base != "" {
			p = base + "/" + rel
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
	if len(s) != 40 {
		return plumbing.ZeroHash, false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return plumbing.ZeroHash, false
		}
	}
	return plumbing.NewHash(strings.ToLower(s)), true
}

// changesCursor is the whole state of one paged read. It is stateless on the
// server by design — nothing is stored, nothing expires — because the answer
// is a pure function of (since, head, prefix). The MCP tool and the REST route
// share it, so a cursor minted by one is accepted by the other.
type changesCursor struct {
	Since  string `json:"s,omitempty"`
	Head   string `json:"h"`
	Prefix string `json:"p,omitempty"`
	After  string `json:"a"`
}

// EncodeChangesCursor returns the opaque token for the page after `after`.
func EncodeChangesCursor(since, head, prefix, after string) string {
	b, _ := json.Marshal(changesCursor{Since: since, Head: head, Prefix: prefix, After: after})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeChangesCursor reverses EncodeChangesCursor. The returned query has no
// Limit; the caller sets one.
func DecodeChangesCursor(s string) (ChangesQuery, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ChangesQuery{}, ErrInvalidChangesCursor
	}
	var c changesCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Head == "" || c.After == "" {
		return ChangesQuery{}, ErrInvalidChangesCursor
	}
	return ChangesQuery{Since: c.Since, Head: c.Head, Prefix: c.Prefix, After: c.After}, nil
}
