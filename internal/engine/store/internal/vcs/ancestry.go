package vcs

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
)

// CommitParentsAndTime returns a commit's parent hashes and its committer
// time (Unix seconds), read from the git objects.
func (rh *Repo) CommitParentsAndTime(hash string) ([]string, int64, error) {
	c, err := rh.repo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		return nil, 0, fmt.Errorf("CommitParentsAndTime: %s: %w", hash, err)
	}
	parents := make([]string, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		parents[i] = p.String()
	}
	return parents, c.Committer.When.Unix(), nil
}
