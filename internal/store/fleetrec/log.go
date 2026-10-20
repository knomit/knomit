package fleetrec

import (
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/store/internal/vcs"
)

// Storer is what the readers need of a repository's storage: its objects and
// its references. The store's git storer satisfies it.
type Storer interface {
	storer.EncodedObjectStorer
	storer.ReferenceStorer
}

// At returns everything the judge reads from a mounted fleet repository at
// branch, all from the ONE commit branch names: the member records and the
// certificate artifacts. Reading them separately could pair the records of
// one revision with the CRL of another. root is the store's fact root.
func At(st Storer, branch, root string) (Fleet, error) {
	ref, err := st.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %s: %w", branch, err)
	}
	c, err := object.GetCommit(st, ref.Hash())
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	return LoadFleetAtCommit("mounted", c, root)
}

// FileAt reads the regular file p (a repo-root path) at branch's tip: ok is
// false when there is none (a symlink counts as none). A blob larger than
// limit is an error and is not read.
func FileAt(st Storer, branch, p string, limit int64) ([]byte, bool, error) {
	ref, err := st.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", branch, err)
	}
	c, err := object.GetCommit(st, ref.Hash())
	if err != nil {
		return nil, false, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, false, err
	}
	return TreeBytesCapped(tree, p, limit)
}

// KeysOf returns every key agentID's member record has held in the history of
// branch: the record's VERSIONS, which is where an agent's key history lives.
// Malformed versions are skipped. root is the store's fact root ("" = "kb").
func KeysOf(st Storer, branch, root, agentID string) ([]ssh.PublicKey, error) {
	ref, err := st.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil, fmt.Errorf("fleet: %s: %w", branch, err)
	}
	if root == "" {
		root = "kb"
	}
	var keys []ssh.PublicKey
	seen := map[string]bool{}
	err = vcs.WalkHistory(st, ref.Hash(), nil, func(c *object.Commit) {
		ms, lerr := LoadMembers("mounted", c, root)
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

// Since is, for each repo path in paths that branch's tip holds, the
// committer time of the oldest commit of branch's first-parent chain from
// which that path's blob is the tip's: when the current version landed on
// main. A path whose current version is older than the maxCommits commits
// walked is left out, and so is one the tip does not hold.
func Since(st Storer, branch string, paths []string, maxCommits int) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	pending := map[string]plumbing.Hash{}
	first := true
	err := walkFirstParent(st, branch, maxCommits, func(c *object.Commit, parent *object.Commit) error {
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
func walkFirstParent(st Storer, branch string, max int, visit func(c, parent *object.Commit) error) error {
	ref, err := st.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return fmt.Errorf("fleet: %s: %w", branch, err)
	}
	c, err := object.GetCommit(st, ref.Hash())
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

// MainLog returns the first-parent commits of branch (newest first) that add
// rootCert (the fleet ontology's pki: root path), change the host record or a
// member record under <root>/members/, or merge a branch, walking at most
// maxCommits commits (walkFirstParent) and stopping once maxEntries are
// found: the walk is newest first, so a caller that shows only the newest n
// events needs no more than n entries. root is the store's fact root
// ("" = "kb").
func MainLog(st Storer, branch, root, rootCert string, maxCommits, maxEntries int) ([]MainCommit, error) {
	if root == "" {
		root = "kb"
	}
	members := path.Join(root, fact.MembersTopic)
	var out []MainCommit
	err := walkFirstParent(st, branch, maxCommits, func(c *object.Commit, parent *object.Commit) error {
		mc := MainCommit{Hash: c.Hash.String(), At: c.Committer.When.UTC(), Author: vcs.AgentIDOfAuthor(c.Author.Email)}
		if c.NumParents() >= 2 {
			p2, err := c.Parent(1)
			if err != nil {
				return err
			}
			mc.MergedTip, mc.MergedBy = p2.Hash.String(), vcs.AgentIDOfAuthor(p2.Author.Email)
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
			if raw, ok, err := TreeBytesCapped(tree, fact.FleetHostPath, 64<<10); err == nil && ok {
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
