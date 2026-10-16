package store

import (
	"errors"
	"fmt"
	"math"
	"time"

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

// FleetSince is, for each repo path in paths that branch's tip holds, the
// committer time of the oldest commit of branch's first-parent chain from
// which that path's blob is the tip's: when the current version landed on
// main. A path whose current version is older than the maxCommits commits
// walked is left out, and so is one the tip does not hold.
func (s *Service) FleetSince(branch string, paths []string, maxCommits int) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	err := s.fleetLanded(branch, paths, maxCommits, func(p string, _ int, c *object.Commit) {
		out[p] = c.Committer.When.UTC()
	})
	return out, err
}

// FleetLandedDepth is, for each repo path in paths that branch's tip holds,
// how many first-parent commits back from the tip its current version landed
// (FleetSince's commit, counted: 0 is the tip itself). The walk is not
// bounded, so every path the tip holds is in the answer; one it does not hold
// is left out. Of two paths, the smaller depth landed later.
func (s *Service) FleetLandedDepth(branch string, paths []string) (map[string]int, error) {
	out := map[string]int{}
	err := s.fleetLanded(branch, paths, math.MaxInt, func(p string, depth int, _ *object.Commit) {
		out[p] = depth
	})
	return out, err
}

// fleetLanded walks branch's first-parent chain from its tip, at most
// maxCommits commits, and calls found once for each path of paths the tip
// holds, with the oldest commit of the chain from which that path's blob is
// the tip's and its depth (0: the tip).
func (s *Service) fleetLanded(branch string, paths []string, maxCommits int, found func(p string, depth int, c *object.Commit)) error {
	pending := map[string]plumbing.Hash{}
	depth := -1
	return s.walkFirstParent(branch, maxCommits, func(c *object.Commit, parent *object.Commit) error {
		depth++
		tree, err := c.Tree()
		if err != nil {
			return err
		}
		if depth == 0 {
			for _, p := range paths {
				if h := fileHash(tree, p); !h.IsZero() {
					pending[p] = h
				}
			}
		}
		var ptree *object.Tree
		if parent != nil {
			if ptree, err = parent.Tree(); err != nil {
				return err
			}
		}
		for p, h := range pending {
			if fileHash(ptree, p) != h {
				found(p, depth, c)
				delete(pending, p)
			}
		}
		if len(pending) == 0 {
			return errStopWalk
		}
		return nil
	})
}

// errStopWalk ends walkFirstParent early, without an error.
var errStopWalk = errors.New("stop")

// walkFirstParent visits branch's tip and its first parents, newest first,
// at most max commits, each with its first parent (nil for a root commit).
func (s *Service) walkFirstParent(branch string, max int, visit func(c, parent *object.Commit) error) error {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return fmt.Errorf("fleet: %s: %w", branch, err)
	}
	c, err := object.GetCommit(s.rh.gits, ref.Hash())
	if err != nil {
		return err
	}
	for n := 0; c != nil && n < max; n++ {
		var parent *object.Commit
		if c.NumParents() > 0 {
			if parent, err = c.Parent(0); err != nil {
				return err
			}
		}
		if err := visit(c, parent); err != nil {
			if errors.Is(err, errStopWalk) {
				return nil
			}
			return err
		}
		c = parent
	}
	return nil
}

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
