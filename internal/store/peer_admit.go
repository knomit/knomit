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

// AdmitPeerBranch is where a fetched member branch enters this repository.
// The commits of tip ∖ the consensus branch are judged with receive-pack's
// core: the private-path closure, then judgeIncoming against the fleet at the
// consensus branch — which, the caller being the fleet repository's master,
// is this repository itself. There is no pusher on a fetch, so there is no
// signer = pusher rule; every other rule (signature, records, rules A/B,
// artifact rules) is the push's.
//
// Admitted: refs/heads/<branch> is set to tip as receive-pack's register sets
// it (a new branch seeded from the consensus branch, then moved; a rewind is
// allowed, as on a push), through the commit chokepoint, which kicks the
// consensus merger. Returns "" then. Refused: nothing moves, and the refusal
// text is returned. err is for what could not be judged at all.
//
// The caller decides who may call this (only the fleet's master); the store
// checks only the branch's shape: an agent/* branch that is neither the
// consensus branch nor own.
func (s *Service) AdmitPeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash) (refusal string, err error) {
	rh := s.rh
	upstream := s.UpstreamBranch()
	switch {
	case upstream == "":
		return "", fmt.Errorf("AdmitPeerBranch: %w", ErrNoConsensusBranch)
	case !strings.HasPrefix(branch, "agent/") || branch == upstream || branch == own:
		return "", fmt.Errorf("AdmitPeerBranch: %q is not a member's agent branch", branch)
	case tip.IsZero():
		return "", fmt.Errorf("AdmitPeerBranch: the tip of %s is required", branch)
	}
	uh, err := rh.resolveRef(ctx, upstream)
	if err != nil {
		return "", fmt.Errorf("AdmitPeerBranch: %s: %w", upstream, err)
	}
	onMain := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, uh, nil, func(c *object.Commit) { onMain[c.Hash] = true }); err != nil {
		return "", fmt.Errorf("AdmitPeerBranch: %w", err)
	}
	var commits []*object.Commit
	if err := walkHistory(rh.gits, tip, onMain, func(c *object.Commit) { commits = append(commits, c) }); err != nil {
		return "", fmt.Errorf("AdmitPeerBranch: %w", err)
	}
	if msg := rh.privatePathClosure(ctx, rh.gits, upstream, commits); msg != "" {
		return msg, nil
	}
	lg := log.With().Str("repo", rh.name).Str("branch", branch).Logger()
	self := func(context.Context) (Fleet, error) { return s.FleetAt(upstream) }
	if msg := rh.judgeIncoming(ctx, upstream, self, nil, commits, lg); msg != "" {
		return msg, nil
	}
	return "", rh.promotePeer(ctx, branch, upstream, tip)
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
