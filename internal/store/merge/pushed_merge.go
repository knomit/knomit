// Merging a peer's pushed branch into this instance's agent branch (F11 UI
// merge). A peer pushes its own agent branch to this host over /git (F11);
// here it is merged into the host's own agent branch — by the operator from
// the web UI, or by the consensus merger under `consensus: auto` — and the
// upstream follows that branch.
package merge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
	"knomit/internal/store/internal/vcs"
)

// ErrBranchMoved: the pushed branch no longer points at the commit the caller
// confirmed. Match with errors.Is; errors.As a *BranchMovedError for the two
// hashes.
var ErrBranchMoved = errors.New("branch moved since it was reviewed")

// ErrUnrelatedHistories: the two branches share no commit, so there is no
// merge base to merge from.
var ErrUnrelatedHistories = errors.New("unrelated histories")

// ErrPrivatePathMerge: merging a pushed branch would change a private path
// (fact.IsPrivatePath — .knomit/, any dot segment) of the host's agent branch.
// Match with errors.Is; errors.As a *PrivatePathMergeError for the paths.
var ErrPrivatePathMerge = errors.New("the merge would change private paths")

// PrivatePathMergeError is MergePushed's and MergeConsensus's refusal when the
// merge RESULT's private paths differ from dst's. Nothing was written.
//
// This is the authority for "a peer never changes the host's .knomit/": it
// judges the state the merge would produce, so it covers every way a pushed
// branch can carry a dot change — a commit receive-pack did not judge (a tip
// the host already held, a branch pushed before the push-time closure) as
// well as one it did. It judges current state, not history: a branch that
// only LAGS behind passes once it merges the newest upstream; a branch whose
// own history changed a dot path keeps that change through any merge and
// passes only once it reverts it.
type PrivatePathMergeError struct {
	Src, Dst, SrcTip string
	Paths            []string
}

func (e *PrivatePathMergeError) Error() string {
	return fmt.Sprintf("%s: merging %s at %s into %s would change %s", ErrPrivatePathMerge.Error(), e.Src, e.SrcTip, e.Dst, strings.Join(e.Paths, ", "))
}

func (e *PrivatePathMergeError) Is(target error) bool { return target == ErrPrivatePathMerge }

// privatePathsChanged lists, sorted, the private paths whose content differs
// between two trees.
func (rh *Repo) privatePathsChanged(from, to plumbing.Hash) ([]string, error) {
	ft, err := object.GetTree(rh.vr.Storer(), from)
	if err != nil {
		return nil, err
	}
	tt, err := object.GetTree(rh.vr.Storer(), to)
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTree(ft, tt)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, ch := range changes {
		for _, name := range []string{ch.From.Name, ch.To.Name} {
			if name != "" && fact.IsPrivatePath(name) && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// BranchMovedError is MergePushed's refusal when src is not at srcTip. Nothing
// was written: the caller shows the new tip and asks again.
type BranchMovedError struct {
	Branch   string
	Expected string
	Actual   string
}

func (e *BranchMovedError) Error() string {
	return fmt.Sprintf("%s: %s is at %s, not the reviewed %s", ErrBranchMoved.Error(), e.Branch, e.Actual, e.Expected)
}

func (e *BranchMovedError) Is(target error) bool { return target == ErrBranchMoved }

// onlyMergeCommits reports whether src brings at least one commit dst lacks
// and every such commit has two or more parents. A src already in dst brings
// none and answers false: that case is the reachable-tip no-op's, and this
// rule must not stand in for it (each is pinned by its own test).
func (rh *Repo) onlyMergeCommits(src, dst plumbing.Hash) (bool, error) {
	inDst := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.vr.Storer(), dst, nil, func(c *object.Commit) { inDst[c.Hash] = true }); err != nil {
		return false, err
	}
	if inDst[src] {
		return false, nil
	}
	only, n := true, 0
	if err := vcs.WalkHistory(rh.vr.Storer(), src, inDst, func(c *object.Commit) {
		n++
		if len(c.ParentHashes) < 2 {
			only = false
		}
	}); err != nil {
		return false, err
	}
	return only && n > 0, nil
}

// PushedBranchInfo is what the pushed-branches list shows for one branch.
type PushedBranchInfo struct {
	Name      string
	Tip       string
	TipTime   time.Time
	TipAuthor string
	// ToMerge counts commits reachable from Tip and not from the agent
	// branch: what a merge would bring in. 0 once merged.
	ToMerge int
	// MergeBase is the best common ancestor of Tip and the agent branch ("" if
	// the histories are unrelated): the `since` for the changes list.
	MergeBase string
	// OtherFilesChanged counts paths outside the ontology root that differ
	// between MergeBase (or nothing) and Tip — the changes list shows facts
	// only.
	OtherFilesChanged int
}

// PushedBranches reports each named branch against agentBranch. The caller
// decides which branches are pushed ones (repos.RepoInstance.IsPushedBranch);
// this only reads. The agent branch's history is walked once for all of them.
func (rh *Repo) PushedBranches(ctx context.Context, names []string, agentBranch, ontologyRoot string) ([]PushedBranchInfo, error) {
	agentRef, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(agentBranch))
	if err != nil {
		return nil, fmt.Errorf("pushed branches: agent branch %q: %w", agentBranch, err)
	}
	agentCommit, err := object.GetCommit(rh.vr.Storer(), agentRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("pushed branches: agent tip: %w", err)
	}
	inAgent := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.vr.Storer(), agentRef.Hash(), nil, func(c *object.Commit) { inAgent[c.Hash] = true }); err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(ontologyRoot, "/") + "/"

	out := make([]PushedBranchInfo, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(name))
		if err != nil {
			return nil, fmt.Errorf("pushed branches: %q: %w", name, err)
		}
		tip, err := object.GetCommit(rh.vr.Storer(), ref.Hash())
		if err != nil {
			return nil, fmt.Errorf("pushed branches: %q tip: %w", name, err)
		}
		info := PushedBranchInfo{
			Name: name, Tip: tip.Hash.String(),
			TipTime: tip.Committer.When, TipAuthor: tip.Author.Name,
		}
		if !inAgent[tip.Hash] {
			if err := vcs.WalkHistory(rh.vr.Storer(), tip.Hash, inAgent, func(*object.Commit) { info.ToMerge++ }); err != nil {
				return nil, err
			}
		}
		var baseTree *object.Tree
		if bases, err := tip.MergeBase(agentCommit); err == nil && len(bases) > 0 {
			info.MergeBase = bases[0].Hash.String()
			if baseTree, err = bases[0].Tree(); err != nil {
				return nil, err
			}
		}
		tipTree, err := tip.Tree()
		if err != nil {
			return nil, err
		}
		changes, err := object.DiffTree(baseTree, tipTree)
		if err != nil {
			return nil, fmt.Errorf("pushed branches: %q diff: %w", name, err)
		}
		for _, ch := range changes {
			p := ch.To.Name
			if p == "" {
				p = ch.From.Name
			}
			if !strings.HasPrefix(p, prefix) {
				info.OtherFilesChanged++
			}
		}
		out = append(out, info)
	}
	return out, nil
}
