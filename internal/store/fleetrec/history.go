package fleetrec

import (
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The fleet repository's main, read as its history: what the Fleet page's
// Members (since) and Activity tabs show. Both walk main's FIRST-PARENT chain
// from its tip, newest first, and at most maxCommits commits: on a fleet
// repository that chain is the master's own commits (create, enroll, revoke,
// the host record) and its merges of members' branches (the second parent
// is the member's tip), so each first-parent commit diffed against its first
// parent is one thing that happened on main.

// errStopWalk ends walkFirstParent early, without an error.
var errStopWalk = errors.New("stop")

// fileHash is the blob hash of the regular file p in tree: zero when tree is
// nil, or p is absent, unreadable or not a regular file.
func fileHash(tree *object.Tree, p string) plumbing.Hash {
	if tree == nil {
		return plumbing.ZeroHash
	}
	e, err := tree.FindEntry(p)
	if err != nil || !e.Mode.IsFile() {
		return plumbing.ZeroHash
	}
	return e.Hash
}
