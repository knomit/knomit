package store

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/store/internal/vcs"
)

// commitTreeShape is CheckTreeShape over a commit's tree and every parent's
// tree: the gate's rules diff a commit against its parents (recordOwner,
// artifactRules) and M3 flattens both parents, and a parent tree that
// truncates truncates the diff.
//
// A malformed tree already on main wedges the gate: every later commit has it
// as a parent and is refused, the repair included, on every hosting and in
// every verify_signatures mode (the receive-pack closure checks parents too).
// The way out is a write that no gate judges. On a knomit host that is a
// host-local write to main (factIndex.deleteFile of the bogus entry's path:
// fileExists finds the entry by name and deletePath drops it whatever its
// mode). Only the immediate parents are checked, so commits on top of the
// repair are admitted again. On a forge it is a push to main past the
// required `knomit verify ci` check. Neither repairs commit_log:
// PopulateCommitLog cannot diff the malformed commit, so every later
// derivation on that branch still fails. A fresh repository is the clean
// recovery.
func commitTreeShape(c *object.Commit, ok map[plumbing.Hash]bool) error {
	tree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("tree %s: %w", c.TreeHash, err)
	}
	if err := vcs.CheckTreeShape(tree, ok); err != nil {
		return err
	}
	for i, ph := range c.ParentHashes {
		p, err := c.Parent(i)
		if err != nil {
			return fmt.Errorf("parent %s: %w", ph, err)
		}
		pt, err := p.Tree()
		if err != nil {
			return fmt.Errorf("parent %s: tree %s: %w", ph, p.TreeHash, err)
		}
		if err := vcs.CheckTreeShape(pt, ok); err != nil {
			return fmt.Errorf("parent %s: %w", ph, err)
		}
	}
	return nil
}
