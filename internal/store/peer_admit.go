// The fleet master's fetch-time entry (R-merge, round 3): on a fleet whose
// copy at the master has an origin (a forge), members' branches do not arrive
// by push. The master fetches them (SyncWithPeers, refs/remotes/origin/agent/*),
// and each one ENTERS this repository here, judged exactly as receive-pack
// judges a push (judgeIncoming, privatePathClosure), and only an admitted tip
// becomes a local refs/heads/agent/<x> — the same shape a push leaves on a
// knomit host, so the consensus merger and everything after it is unchanged.
// A refused tip stays a remote-tracking ref nothing reads: "left behind".
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"
)

const peerTrackingPrefix = "refs/remotes/origin/agent/"

// PeerTips lists the fetched agent/* branches (refs/remotes/origin/agent/*),
// by local branch name ("agent/<x>"), except own (this instance's agent
// branch, which the ordinary reconcile owns).
func (ri *remoteIndex) PeerTips(own string) (map[string]plumbing.Hash, error) {
	iter, err := ri.rh.gits.IterReferences()
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

// Tip is the consensus branch's tip this history was read at.
func (h *MainHistory) Tip() plumbing.Hash { return h.tip }

// MainHistory reads the consensus branch's tip and its history.
func (s *Service) MainHistory(ctx context.Context) (*MainHistory, error) {
	upstream := s.UpstreamBranch()
	if upstream == "" {
		return nil, fmt.Errorf("MainHistory: %w", ErrNoConsensusBranch)
	}
	uh, err := s.rh.resolveRef(ctx, upstream)
	if err != nil {
		return nil, fmt.Errorf("MainHistory: %s: %w", upstream, err)
	}
	onMain := map[plumbing.Hash]bool{}
	if err := walkHistory(s.rh.gits, uh, nil, func(c *object.Commit) { onMain[c.Hash] = true }); err != nil {
		return nil, fmt.Errorf("MainHistory: %w", err)
	}
	return &MainHistory{tip: uh, onMain: onMain}, nil
}

// AdmitPeerBranch is where a fetched member branch enters this repository.
// It judges tip with JudgePeerBranch and, admitted, promotes it.
//
// Admitted: refs/heads/<branch> is set to tip as receive-pack's register sets
// it (a new branch seeded from the consensus branch, then moved; a rewind is
// allowed, as on a push), through the commit chokepoint, which kicks the
// consensus merger. Returns "" then. Refused: nothing moves, and the refusal
// text is returned. err is for what could not be judged at all.
//
// main is the consensus branch's history (MainHistory); nil reads it here.
// The caller decides who may call this (only the fleet's master); the store
// checks only the branch's shape: an agent/* branch that is neither the
// consensus branch nor own.
func (s *Service) AdmitPeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash, main *MainHistory) (refusal string, err error) {
	refusal, err = s.JudgePeerBranch(ctx, branch, own, tip, main)
	if err != nil || refusal != "" {
		return refusal, err
	}
	return "", s.rh.promotePeer(ctx, branch, s.UpstreamBranch(), tip)
}

// JudgePeerBranch judges a member branch's tip without moving anything: the
// commits of tip ∖ main (EVERY one, not the tip alone), with receive-pack's
// core: the private-path closure, then judgeIncoming against the fleet at
// the consensus branch — which, the caller being the fleet repository's
// master, is this repository itself. There is no pusher, so there is no
// signer = pusher rule; every other rule (signature, records, rules A/B,
// artifact rules) is the push's. Returns the refusal text, or "".
//
// It is the fetch judge (AdmitPeerBranch) and the master's re-judge of a
// local agent/* branch when main has moved since it was admitted (the
// merger's, on both hostings: a revocation on main can turn an admitted,
// unmerged tip into a refused one).
func (s *Service) JudgePeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash, main *MainHistory) (string, error) {
	rh := s.rh
	upstream := s.UpstreamBranch()
	switch {
	case upstream == "":
		return "", fmt.Errorf("JudgePeerBranch: %w", ErrNoConsensusBranch)
	case !strings.HasPrefix(branch, "agent/") || branch == upstream || branch == own:
		return "", fmt.Errorf("JudgePeerBranch: %q is not a member's agent branch", branch)
	case tip.IsZero():
		return "", fmt.Errorf("JudgePeerBranch: the tip of %s is required", branch)
	}
	if main == nil {
		var err error
		if main, err = s.MainHistory(ctx); err != nil {
			return "", err
		}
	}
	var commits []*object.Commit
	if err := walkHistory(rh.gits, tip, main.onMain, func(c *object.Commit) { commits = append(commits, c) }); err != nil {
		return "", fmt.Errorf("JudgePeerBranch: %w", err)
	}
	if msg := rh.privatePathClosure(ctx, rh.gits, upstream, commits); msg != "" {
		return msg, nil
	}
	lg := log.With().Str("repo", rh.name).Str("branch", branch).Logger()
	self := func(context.Context) (Fleet, error) { return s.FleetAt(upstream) }
	return rh.judgeIncoming(ctx, upstream, self, nil, commits, lg), nil
}

// DemotePeerBranch removes the local refs/heads/<branch> when it is still at
// tip: the fleet master's answer to a re-judge that refused a branch it had
// admitted (PR #524 N3), so the merger cannot merge it. A branch that moved
// meanwhile (a push or a promotion of a newer tip) is left alone, and
// reported false. Only an agent/* branch other than the consensus branch and
// own is ever touched. The objects stay; on the fleet repository a held
// commit no admitted ref reaches is judged again by receive-pack (heldAdmitted).
func (s *Service) DemotePeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash) (bool, error) {
	rh := s.rh
	upstream := s.UpstreamBranch()
	if !strings.HasPrefix(branch, "agent/") || branch == upstream || branch == own {
		return false, fmt.Errorf("DemotePeerBranch: %q is not a member's agent branch", branch)
	}
	id, err := rh.branchID(ctx, branch)
	if err != nil {
		return false, nil // not a registered branch: nothing to demote
	}
	defer rh.lockBranch(branch)()
	ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("DemotePeerBranch: read %s: %w", branch, err)
	}
	if ref.Hash() != tip {
		return false, nil
	}
	if err := rh.dropBranchLocked(context.WithoutCancel(ctx), branch, id); err != nil {
		return false, fmt.Errorf("DemotePeerBranch: %w", err)
	}
	return true, nil
}

// promotePeer sets refs/heads/<branch> to tip, as receivePack.register does
// for a push, minus the objects (a fetch already stored them) and the
// old-hash check (nothing advertised a value to compare).
func (rh *repoHandler) promotePeer(ctx context.Context, branch, upstream string, tip plumbing.Hash) error {
	defer rh.lockBranch(branch)()
	ctx = context.WithoutCancel(ctx)
	seeded := false
	if _, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch)); errors.Is(err, plumbing.ErrReferenceNotFound) {
		if err := rh.CreateBranch(ctx, branch, upstream); err != nil {
			return fmt.Errorf("AdmitPeerBranch: seed %s: %w", branch, err)
		}
		seeded = true
	} else if err != nil {
		return fmt.Errorf("AdmitPeerBranch: read %s: %w", branch, err)
	}
	res, _, err := rh.advanceBranchTo(ctx, branch, tip)
	if err != nil {
		return fmt.Errorf("AdmitPeerBranch: %w", err)
	}
	if seeded && res.Mode == ModeNoop {
		return rh.notifyCommit(ctx, branch, tip)
	}
	return nil
}
