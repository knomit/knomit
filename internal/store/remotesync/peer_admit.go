// The fleet master's fetch-time entry (R-merge, round 3): on a fleet whose
// copy at the master has an origin (a forge), members' branches do not arrive
// by push. The master fetches them (SyncWithPeers, refs/remotes/origin/agent/*),
// and each one ENTERS this repository here, judged exactly as receive-pack
// judges a push (judgeIncoming, privatePathClosure), and only an admitted tip
// becomes a local refs/heads/agent/<x> — the same shape a push leaves on a
// knomit host, so the consensus merger and everything after it is unchanged.
// A refused tip stays a remote-tracking ref nothing reads: "left behind".
package remotesync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/store/fleetrec"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
	"knomit/internal/store/verify"
)

const peerTrackingPrefix = "refs/remotes/origin/agent/"

// PeerTips lists the fetched agent/* branches (refs/remotes/origin/agent/*),
// by local branch name ("agent/<x>"), except own (this instance's agent
// branch, which the ordinary reconcile owns).
func (ri *Index) PeerTips(own string) (map[string]plumbing.Hash, error) {
	iter, err := ri.rh.Storer().IterReferences()
	if err != nil {
		return nil, fmt.Errorf("PeerTips: %w", err)
	}
	defer iter.Close()
	out := map[string]plumbing.Hash{}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if ref.Type() != plumbing.HashReference || !strings.HasPrefix(name, peerTrackingPrefix) {
			return nil
		}
		branch := "agent/" + strings.TrimPrefix(name, peerTrackingPrefix)
		if branch != own {
			out[branch] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("PeerTips: %w", err)
	}
	return out, nil
}

// MainHistory is the consensus branch at one tip, with every commit
// reachable from it: what AdmitPeerBranch and JudgePeerBranch subtract from
// a member's tip. The fleet master reads it once per round (PR #524 N2) and
// passes it to every branch it judges, so a round costs one walk of main,
// not one per branch.
type MainHistory struct {
	tip    plumbing.Hash
	onMain map[plumbing.Hash]bool
}

// NewMainHistory builds the history of the consensus branch at tip; onMain
// holds every commit reachable from it.
func NewMainHistory(tip plumbing.Hash, onMain map[plumbing.Hash]bool) *MainHistory {
	return &MainHistory{tip: tip, onMain: onMain}
}

// OnMain is the set of commits reachable from the tip. Callers only read it.
func (h *MainHistory) OnMain() map[plumbing.Hash]bool { return h.onMain }

// Tip is the consensus branch's tip this history was read at.
func (h *MainHistory) Tip() plumbing.Hash { return h.tip }

// PromotePeer sets refs/heads/<branch> to tip, as receivePack.register does
// for a push, minus the objects (a fetch already stored them) and the
// old-hash check (nothing advertised a value to compare).
func (rh *Repo) PromotePeer(ctx context.Context, branch, upstream string, tip plumbing.Hash) error {
	defer rh.LockBranch(branch)()
	ctx = context.WithoutCancel(ctx)
	seeded := false
	if _, err := rh.Storer().Reference(plumbing.NewBranchReferenceName(branch)); errors.Is(err, plumbing.ErrReferenceNotFound) {
		if err := rh.CreateBranch(ctx, branch, upstream); err != nil {
			return fmt.Errorf("AdmitPeerBranch: seed %s: %w", branch, err)
		}
		seeded = true
	} else if err != nil {
		return fmt.Errorf("AdmitPeerBranch: read %s: %w", branch, err)
	}
	res, _, err := rh.AdvanceBranchTo(ctx, branch, tip)
	if err != nil {
		return fmt.Errorf("AdmitPeerBranch: %w", err)
	}
	if seeded && res.Mode == merge.ModeNoop {
		return rh.NotifyCommit(ctx, branch, tip)
	}
	return nil
}

// ReadMainHistory reads the consensus branch's tip and its history; the
// contract is Service.MainHistory's. upstream is the consensus branch's name
// (the Service resolves it and refuses "").
func (rh *Repo) ReadMainHistory(ctx context.Context, upstream string) (*MainHistory, error) {
	uh, err := rh.ResolveRef(ctx, upstream)
	if err != nil {
		return nil, fmt.Errorf("MainHistory: %s: %w", upstream, err)
	}
	onMain := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.Storer(), uh, nil, func(c *object.Commit) { onMain[c.Hash] = true }); err != nil {
		return nil, fmt.Errorf("MainHistory: %w", err)
	}
	return NewMainHistory(uh, onMain), nil
}

// JudgePeerBranch judges a member branch's tip without moving anything; the
// contract is Service.JudgePeerBranch's. upstream is the consensus branch's
// name, already resolved and checked non-empty by the Service. The fleet the
// judge reads is the one at upstream in THIS store (fleetrec.At, which is what
// Service.FleetAt reads).
func (rh *Repo) JudgePeerBranch(ctx context.Context, upstream, branch, own string, tip plumbing.Hash, main *MainHistory) (string, error) {
	switch {
	case !strings.HasPrefix(branch, "agent/") || branch == upstream || branch == own:
		return "", fmt.Errorf("JudgePeerBranch: %q is not a member's agent branch", branch)
	case tip.IsZero():
		return "", fmt.Errorf("JudgePeerBranch: the tip of %s is required", branch)
	}
	if main == nil {
		var err error
		if main, err = rh.ReadMainHistory(ctx, upstream); err != nil {
			return "", err
		}
	}
	var commits []*object.Commit
	if err := vcs.WalkHistory(rh.Storer(), tip, main.OnMain(), func(c *object.Commit) { commits = append(commits, c) }); err != nil {
		return "", fmt.Errorf("JudgePeerBranch: %w", err)
	}
	if msg := verify.PrivatePathClosure(ctx, rh.Repo.Repo, rh.Storer(), upstream, commits); msg != "" {
		return msg, nil
	}
	lg := log.With().Str("repo", rh.Name()).Str("branch", branch).Logger()
	self := func(context.Context) (fleetrec.Fleet, error) {
		return fleetrec.At(rh.Storer(), upstream, rh.FactRoot())
	}
	return verify.JudgeIncoming(ctx, rh.Repo.Repo, upstream, self, nil, tip, commits, lg), nil
}

// DemotePeerBranch removes the local refs/heads/<branch> when it is still at
// tip; the contract is Service.DemotePeerBranch's. upstream is the consensus
// branch's name as the Service resolved it.
func (rh *Repo) DemotePeerBranch(ctx context.Context, upstream, branch, own string, tip plumbing.Hash) (bool, error) {
	if !strings.HasPrefix(branch, "agent/") || branch == upstream || branch == own {
		return false, fmt.Errorf("DemotePeerBranch: %q is not a member's agent branch", branch)
	}
	id, err := rh.BranchID(ctx, branch)
	if err != nil {
		return false, nil // not a registered branch: nothing to demote
	}
	defer rh.LockBranch(branch)()
	ref, err := rh.Storer().Reference(plumbing.NewBranchReferenceName(branch))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("DemotePeerBranch: read %s: %w", branch, err)
	}
	if ref.Hash() != tip {
		return false, nil
	}
	if err := rh.DropBranchLocked(context.WithoutCancel(ctx), branch, id); err != nil {
		return false, fmt.Errorf("DemotePeerBranch: %w", err)
	}
	return true, nil
}
