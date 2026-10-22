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
package vcs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/model"
)

// Sentinel errors a changes read can return. Each one is a CLIENT error with a
// named remedy: none of them may ever be turned into an empty answer, because
// an empty answer reads as "nothing changed" and would be believed.
var ()

// Change values on a PathChange.
const ()

// NormalizeChangesPrefix turns a caller's folder into the repo path it names.
//
// The prefix is RELATIVE TO THE ONTOLOGY ROOT (`tasks/a` means
// `<OntologyRoot>/tasks/a`), lowercased because knomit writes lowercase fact
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
		return "", fmt.Errorf("%w: %q contains \"..\"", model.ErrInvalidPrefix, prefix)
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("%w: %q has an empty path segment", model.ErrInvalidPrefix, prefix)
	}
	if fact.IsPrivatePath(p) {
		return "", fmt.Errorf("%w: %q names private state (a segment beginning with \".\")", model.ErrInvalidPrefix, prefix)
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
func (rh *Repo) ChangesUnder(ctx context.Context, branch string, q model.ChangesQuery) (model.ChangesResult, error) {
	base, err := NormalizeChangesPrefix(rh.OntologyRoot(), q.Prefix)
	if err != nil {
		return model.ChangesResult{}, err
	}

	tip, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return model.ChangesResult{}, err
	}
	tipCommit, err := rh.repo.CommitObject(tip)
	if err != nil {
		return model.ChangesResult{}, fmt.Errorf("ChangesUnder: tip commit: %w", err)
	}

	headCommit := tipCommit
	if q.Head != "" {
		// A pinned head comes from a paging cursor. It must be a commit on
		// this branch's history — otherwise a cursor minted on one branch
		// could replay another branch's tree here.
		h, ok := model.ParseFullHash(q.Head)
		if !ok {
			return model.ChangesResult{}, model.ErrInvalidChangesCursor
		}
		c, err := rh.repo.CommitObject(h)
		if err != nil {
			return model.ChangesResult{}, model.ErrInvalidChangesCursor
		}
		if c.Hash != tipCommit.Hash {
			behind, err := c.IsAncestor(tipCommit)
			if err != nil {
				return model.ChangesResult{}, fmt.Errorf("ChangesUnder: cursor ancestry: %w", err)
			}
			if !behind {
				return model.ChangesResult{}, model.ErrInvalidChangesCursor
			}
		}
		headCommit = c
	}

	var fromTree *object.Tree
	if q.Since != "" {
		h, ok := model.ParseFullHash(q.Since)
		if !ok {
			return model.ChangesResult{}, model.ErrUnknownSince
		}
		sinceCommit, err := rh.repo.CommitObject(h)
		if err != nil {
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				return model.ChangesResult{}, model.ErrUnknownSince
			}
			return model.ChangesResult{}, fmt.Errorf("ChangesUnder: since commit: %w", err)
		}
		if sinceCommit.Hash != headCommit.Hash {
			behind, err := sinceCommit.IsAncestor(headCommit)
			if err != nil {
				return model.ChangesResult{}, fmt.Errorf("ChangesUnder: since ancestry: %w", err)
			}
			if !behind {
				return model.ChangesResult{}, fmt.Errorf("%w (branch %q)", model.ErrSinceNotBehind, branch)
			}
		}
		if fromTree, err = subtreeOrEmpty(sinceCommit, base); err != nil {
			return model.ChangesResult{}, err
		}
	}
	toTree, err := subtreeOrEmpty(headCommit, base)
	if err != nil {
		return model.ChangesResult{}, err
	}

	rows, err := diffSubtrees(fromTree, toTree, base, rh.IsFactPath)
	if err != nil {
		return model.ChangesResult{}, err
	}

	res := model.ChangesResult{Head: headCommit.Hash.String(), Changes: []model.PathChange{}}
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
// ChangesUnder — subtreeOrEmpty, diffSubtrees, IsFactPath, no clock, no
// commit log, no write — with ONE difference: there is NO ancestry check. The
// dispatcher's advance is "old head → current head" of the agent branch, and
// after an origin rewind the old head is NOT an ancestor of the new one (the
// agent's commits were replayed under new hashes); the two trees are still
// meaningful, and refusing them would silently lose the fires. ChangesUnder
// keeps its check: its `since` is a client's bookmark, where a sideways diff
// fabricates deletions. A ZeroHash `from` is the empty tree.
func (rh *Repo) DiffFacts(ctx context.Context, from, to plumbing.Hash) ([]model.PathChange, error) {
	base := rh.OntologyRoot()
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
	return diffSubtrees(fromTree, toTree, base, rh.IsFactPath)
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
// Repo.IsFactPath — the indexer's own membership rule (under the
// ontology root, .md, not private) — so changes and the index agree on what a
// fact is, with one copy of the rule.
func diffSubtrees(from, to *object.Tree, base string, isFact func(string) bool) ([]model.PathChange, error) {
	if from == nil && to == nil {
		return nil, nil
	}
	changes, err := object.DiffTree(from, to)
	if err != nil {
		return nil, fmt.Errorf("ChangesUnder: diff: %w", err)
	}
	rows := make([]model.PathChange, 0, len(changes))
	for _, ch := range changes {
		var rel, kind string
		switch {
		case ch.From.Name == "" && ch.To.Name != "":
			rel, kind = ch.To.Name, model.ChangeAdded
		case ch.From.Name != "" && ch.To.Name == "":
			rel, kind = ch.From.Name, model.ChangeDeleted
		default:
			rel, kind = ch.To.Name, model.ChangeModified
		}
		p := rel
		if base != "" {
			p = base + "/" + rel
		}
		if !isFact(p) {
			continue
		}
		rows = append(rows, model.PathChange{Path: p, Change: kind})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows, nil
}
