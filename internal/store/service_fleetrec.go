package store

import (
	"errors"
	"fmt"
	"knomit/internal/fact"
	"path"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"
)

// FleetMembersAt returns the member records at branch in THIS store (a
// mounted fleet repository), after checking that its ontology there is the
// fleet ontology. root is the store's fact root.
func (s *Service) FleetMembersAt(branch string) ([]FleetMember, error) {
	f, err := s.FleetAt(branch)
	return f.Members, err
}

// FleetPKIAt returns the fleet's certificate artifacts at branch in THIS store
// (a mounted fleet repository): nil, nil when that revision declares no
// pki: root or holds no root file.
func (s *Service) FleetPKIAt(branch string) (*FleetPKI, error) {
	f, err := s.FleetAt(branch)
	return f.PKI, err
}

// FleetAt returns everything the judge reads from THIS store (a mounted fleet
// repository) at branch, all from the ONE commit branch names: the member
// records and the certificate artifacts. Reading them separately could pair
// the records of one revision with the CRL of another.
func (s *Service) FleetAt(branch string) (Fleet, error) {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %s: %w", branch, err)
	}
	c, err := object.GetCommit(s.rh.gits, ref.Hash())
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	return loadFleetAt("mounted", c, s.rh.factRoot)
}

// FileAt reads the regular file p (a repo-root path) at branch's tip in THIS
// store: ok is false when there is none (a symlink counts as none). A blob
// larger than limit is an error and is not read.
func (s *Service) FileAt(branch, p string, limit int64) ([]byte, bool, error) {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", branch, err)
	}
	c, err := object.GetCommit(s.rh.gits, ref.Hash())
	if err != nil {
		return nil, false, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, false, err
	}
	return treeBytesCapped(tree, p, limit)
}

// FleetKeysOf returns every key agentID's member record has held in this
// store's (a mounted fleet repository's) history of branch: the record's
// VERSIONS, which is where an agent's key history lives. Malformed versions
// are skipped.
func (s *Service) FleetKeysOf(branch, agentID string) ([]ssh.PublicKey, error) {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil, fmt.Errorf("fleet: %s: %w", branch, err)
	}
	root := s.rh.factRoot
	if root == "" {
		root = "kb"
	}
	var keys []ssh.PublicKey
	seen := map[string]bool{}
	err = walkHistory(s.rh.gits, ref.Hash(), nil, func(c *object.Commit) {
		ms, lerr := loadMembers("mounted", c, root)
		if lerr != nil {
			return
		}
		for _, m := range ms {
			k := string(m.Key.Marshal())
			if m.Agent == agentID && !seen[k] {
				seen[k] = true
				keys = append(keys, m.Key)
			}
		}
	})
	return keys, err
}

// FleetSince is, for each repo path in paths that branch's tip holds, the
// committer time of the oldest commit of branch's first-parent chain from
// which that path's blob is the tip's: when the current version landed on
// main. A path whose current version is older than the maxCommits commits
// walked is left out, and so is one the tip does not hold.
func (s *Service) FleetSince(branch string, paths []string, maxCommits int) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	pending := map[string]plumbing.Hash{}
	first := true
	err := s.walkFirstParent(branch, maxCommits, func(c *object.Commit, parent *object.Commit) error {
		tree, err := c.Tree()
		if err != nil {
			return err
		}
		if first {
			first = false
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
				out[p] = c.Committer.When.UTC()
				delete(pending, p)
			}
		}
		if len(pending) == 0 {
			return errStopWalk
		}
		return nil
	})
	return out, err
}

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

// FleetMainLog returns the first-parent commits of branch (newest first)
// that add rootCert (the fleet ontology's pki: root path), change the host
// record or a member record under the store's <root>/members/, or merge a
// branch, walking at most maxCommits commits (walkFirstParent) and stopping
// once maxEntries are found: the walk is newest first, so a caller that
// shows only the newest n events needs no more than n entries.
func (s *Service) FleetMainLog(branch, rootCert string, maxCommits, maxEntries int) ([]FleetMainCommit, error) {
	root := s.rh.factRoot
	if root == "" {
		root = "kb"
	}
	members := path.Join(root, fact.MembersTopic)
	var out []FleetMainCommit
	err := s.walkFirstParent(branch, maxCommits, func(c *object.Commit, parent *object.Commit) error {
		mc := FleetMainCommit{Hash: c.Hash.String(), At: c.Committer.When.UTC(), Author: agentIDOfAuthor(c.Author.Email)}
		if c.NumParents() >= 2 {
			p2, err := c.Parent(1)
			if err != nil {
				return err
			}
			mc.MergedTip, mc.MergedBy = p2.Hash.String(), agentIDOfAuthor(p2.Author.Email)
		}
		tree, err := c.Tree()
		if err != nil {
			return err
		}
		var ptree *object.Tree
		if parent != nil {
			if ptree, err = parent.Tree(); err != nil {
				return err
			}
		}
		if rootCert != "" {
			mc.RootAdded = !fileHash(tree, rootCert).IsZero() && fileHash(ptree, rootCert).IsZero()
		}
		if fileHash(tree, fact.FleetHostPath) != fileHash(ptree, fact.FleetHostPath) {
			mc.HostChanged = true
			if raw, ok, err := treeBytesCapped(tree, fact.FleetHostPath, 64<<10); err == nil && ok {
				mc.Host, _ = fact.ParseFleetHost(raw)
			}
		}
		if mc.Records, err = recordChanges(ptree, tree, members); err != nil {
			return err
		}
		if mc.RootAdded || mc.HostChanged || mc.MergedTip != "" || len(mc.Records) > 0 {
			out = append(out, mc)
		}
		if len(out) >= maxEntries {
			return errStopWalk
		}
		return nil
	})
	return out, err
}
