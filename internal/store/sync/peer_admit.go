// The fleet master's fetch-time entry (R-merge, round 3): on a fleet whose
// copy at the master has an origin (a forge), members' branches do not arrive
// by push. The master fetches them (SyncWithPeers, refs/remotes/origin/agent/*),
// and each one ENTERS this repository here, judged exactly as receive-pack
// judges a push (judgeIncoming, privatePathClosure), and only an admitted tip
// becomes a local refs/heads/agent/<x> — the same shape a push leaves on a
// knomit host, so the consensus merger and everything after it is unchanged.
// A refused tip stays a remote-tracking ref nothing reads: "left behind".
package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/store/merge"
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
