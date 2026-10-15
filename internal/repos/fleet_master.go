package repos

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog"

	"knomit/internal/store"
)

// The master merges (R-merge, round 3): on the fleet repository only the
// fleet's master merges members' branches into main, on every hosting, and
// the origin is transport only. With no origin (a knomit-hosted fleet, the
// master is the hub) the branches arrive by push through receive-pack and
// main follows through the local reconcile: nothing here runs. With an
// origin (a forge) the master's ordinary origin round does three more things,
// inside the same round, and only while ri.isFleetMaster holds:
//
//  1. its fetch adds every agent/* branch (store SyncWithPeers);
//  2. each fetched member tip that moved since it was last judged ENTERS the
//     repository through the receive-pack judge (store AdmitPeerBranch):
//     admitted, it becomes the local agent/<x> branch, which kicks the
//     consensus merger exactly as a push does; refused, it is left behind and
//     remembered (PeerRefusal, shown as last_refusal on GET /fleet/members)
//     until the tip or main moves;
//  3. after pushing its own agent branch, it fast-forwards local main to that
//     branch (AdvanceLocalUpstream) and pushes main WITHOUT force (store
//     PushUpstream). A non-fast-forward refusal is an ordinary push error;
//     the next round fetches the newer main, merges it into the agent branch
//     and pushes again. Two masters at once is unsupported (the user's job):
//     nothing here detects it.
//
// A member's round is untouched: it fetches main and its own branch, pushes
// its own branch and never merges.

// PeerRefusal is the fleet master's last refusal of one member branch it
// fetched: left behind, not retried until the branch or main moves.
type PeerRefusal struct {
	Branch string    `json:"branch"`
	Tip    string    `json:"tip"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// peerJudged is what a branch was last judged at: its tip, and main's.
type peerJudged struct{ tip, main plumbing.Hash }

// fleetPeers is the fleet master's fetch-time state for one repo. Built with
// the instance (like the consensus merger) and nil-safe.
type fleetPeers struct {
	master func(*store.Service) bool

	mu       sync.Mutex
	judged   map[string]peerJudged
	refusals map[string]PeerRefusal
}

func newFleetPeers(master func(*store.Service) bool) *fleetPeers {
	return &fleetPeers{master: master, judged: map[string]peerJudged{}, refusals: map[string]PeerRefusal{}}
}

// isMaster reports whether this round is the fleet master's; false on nil.
func (p *fleetPeers) isMaster(svc *store.Service) bool {
	return p != nil && p.master != nil && p.master(svc)
}

// isFleetMaster: ri is the fleet repository, mounted through the fleet door,
// and this instance is its master (Manager.isFleetMaster: the root key file
// is here and its certificate is the root on main). The one predicate for
// the merger and for the master's origin round.
func (ri *RepoInstance) isFleetMaster(svc *store.Service) bool {
	return ri.fleetDoor && ri.IsFleet() && ri.env.m.isFleetMaster(svc)
}

// admit judges every fetched member branch that moved since it was last
// judged (step 2 above). Errors that left a branch unjudged are logged and
// retried on the next round.
func (p *fleetPeers) admit(ctx context.Context, svc *store.Service, own string, lg *zerolog.Logger) {
	tips, err := svc.Remote().PeerTips(own)
	if err != nil {
		lg.Warn().Err(err).Msg("fleet master: list the fetched member branches")
		return
	}
	mainTip, err := svc.Branches().HeadCommit(ctx, svc.UpstreamBranch())
	if err != nil {
		lg.Warn().Err(err).Msg("fleet master: read main; member branches not judged this round")
		return
	}
	names := make([]string, 0, len(tips))
	for b := range tips {
		names = append(names, b)
	}
	sort.Strings(names)
	for _, branch := range names {
		if ctx.Err() != nil {
			return
		}
		key := peerJudged{tip: tips[branch], main: plumbing.NewHash(mainTip)}
		p.mu.Lock()
		seen := p.judged[branch] == key
		p.mu.Unlock()
		if seen {
			continue
		}
		reason, err := svc.AdmitPeerBranch(ctx, branch, own, key.tip)
		if err != nil {
			lg.Warn().Err(err).Str("branch", branch).Msg("fleet master: member branch not judged; retried next round")
			continue
		}
		p.mu.Lock()
		p.judged[branch] = key
		if reason != "" {
			p.refusals[branch] = PeerRefusal{Branch: branch, Tip: key.tip.String(), Reason: reason, At: time.Now().UTC()}
		} else {
			delete(p.refusals, branch)
		}
		p.mu.Unlock()
		if reason != "" {
			lg.Warn().Str("branch", branch).Str("tip", shortHash(key.tip.String())).Str("reason", reason).
				Msg("fleet master: member branch refused; left behind")
		} else {
			lg.Info().Str("branch", branch).Str("tip", shortHash(key.tip.String())).
				Msg("fleet master: member branch admitted")
		}
	}
	// A branch gone from the origin is forgotten.
	p.mu.Lock()
	for b := range p.judged {
		if _, ok := tips[b]; !ok {
			delete(p.judged, b)
			delete(p.refusals, b)
		}
	}
	p.mu.Unlock()
}

// publish is step 3: main = the agent branch, pushed without force.
func (p *fleetPeers) publish(ctx context.Context, svc *store.Service, own string, auth transport.AuthMethod) (store.PushResult, error) {
	if _, err := svc.AdvanceLocalUpstream(ctx, own, svc.UpstreamBranch()); err != nil {
		return store.PushResult{}, err
	}
	return svc.Remote().PushUpstream(ctx, auth)
}

// FleetRefusals is the fleet master's last refusal of each member branch it
// fetched and left behind, by branch. Empty on a member, on a knomit-hosted
// master (receive-pack refuses a push to the pusher's face instead), and
// with no fleet repository mounted. Held in memory: a restart judges again.
func (m *Manager) FleetRefusals() map[string]PeerRefusal {
	ri := m.fleetRepo()
	if ri == nil {
		return map[string]PeerRefusal{}
	}
	return ri.peers.Refusals()
}

// Refusals is a copy of the current refusals, by branch; nil-safe.
func (p *fleetPeers) Refusals() map[string]PeerRefusal {
	out := map[string]PeerRefusal{}
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for b, r := range p.refusals {
		out[b] = r
	}
	return out
}
