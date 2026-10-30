package vcs

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
)

// CommitIsAncestor reports whether commit a is an ancestor of commit b (any
// parent), read from the git objects, so it is right for a commit the index
// has not recorded yet (commit_parents and commit_log are filled after a
// commit lands). a == b is not an ancestor.
func (rh *Repo) CommitIsAncestor(a, b string) (bool, error) {
	if a == b {
		return false, nil
	}
	ca, err := rh.repo.CommitObject(plumbing.NewHash(a))
	if err != nil {
		return false, fmt.Errorf("CommitIsAncestor: %s: %w", a, err)
	}
	cb, err := rh.repo.CommitObject(plumbing.NewHash(b))
	if err != nil {
		return false, fmt.Errorf("CommitIsAncestor: %s: %w", b, err)
	}
	return ca.IsAncestor(cb)
}
