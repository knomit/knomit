package repos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

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
//  2. each fetched member tip whose (tip, main) was not judged yet ENTERS
//     the repository through the receive-pack judge (store AdmitPeerBranch):
//     admitted, it becomes the local agent/<x> branch, which kicks the
//     consensus merger exactly as a push does; refused, it is left behind and
//     remembered (PeerRefusal, shown as last_refusal on GET /fleet/members)
//     until the tip or main moves, and a local agent/<x> admitted earlier is
//     demoted (removed) so the merger cannot merge it;
//  3. after pushing its own agent branch, it publishes main (publishFleetMainOn:
//     PublishUpstream WITHOUT force, then local main fast-forwarded to the
//     agent branch). A non-fast-forward refusal is an ordinary push error;
//     the next round fetches the newer main, merges it into the agent branch
//     and pushes again. Two masters at once is unsupported (the user's job):
//     nothing here detects it.
//
// On both hostings the merger re-judges each local agent/* branch against the
// current main before merging it (fleetPeers.mayMerge), with the same memo:
// main moving (a revocation, an issuance) changes every verdict, a branch
// admitted earlier and now refused is demoted, and one refused earlier and
// now admissible is admitted (on a forge the next round promotes it).
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

// peerKey is what a branch's verdict depends on: its tip, and main's. Main
// is part of the key because the fleet the judge reads is main's: a leaf
// issued on main admits a join refused before it (no-cert), a revocation
// refuses a branch admitted before it.
type peerKey struct{ tip, main plumbing.Hash }

// fleetPeers is the fleet master's judging state for one repo: the fetch
// judge's (forge) and the merger's re-judge (both hostings). Built with the
// instance (like the consensus merger) and nil-safe.
type fleetPeers struct {
	master func(*store.Service) bool
	// publishes says whether a push round of this repository publishes main:
	// on the master, and on the holder of the fleet root whose main does not
	// hold it yet (a forge create whose publish of main failed).
	publishes func(*store.Service) bool

	mu sync.Mutex
	// judged is the memo, per branch: the verdict (true = refused) at each
	// (tip, main) judged. Two judges write it, the fetch judge (the branch's
	// tip on the origin) and the merger (the local branch's tip, which can be
	// an older, admitted tip while the fetch refuses a newer one), so it is
	// keyed by tip and never overwritten by the other's verdict. Entries at
	// any other main are dropped when one is recorded: they can never match
	// again.
	judged   map[string]map[peerKey]bool
	refusals map[string]PeerRefusal
	// ring is every refusal recorded, oldest first, the last refusalRing of
	// them: what GET /fleet/activity shows as "refused", including refusals
	// that cleared since (refusals keeps only each branch's current one). In
	// memory only, like refusals.
	ring []PeerRefusal
}

// refusalRing is how many refusals fleetPeers.ring keeps.
const refusalRing = 200

func newFleetPeers(master, publishes func(*store.Service) bool) *fleetPeers {
	return &fleetPeers{master: master, publishes: publishes, judged: map[string]map[peerKey]bool{}, refusals: map[string]PeerRefusal{}}
}

// isMaster reports whether this round is the fleet master's; false on nil.
func (p *fleetPeers) isMaster(svc *store.Service) bool {
	return p != nil && p.master != nil && p.master(svc)
}

// publishesMain reports whether this push round publishes main; false on nil.
func (p *fleetPeers) publishesMain(svc *store.Service) bool {
	return p != nil && p.publishes != nil && p.publishes(svc)
}

// verdictAt is the memoised verdict of branch at key, if it was judged there.
func (p *fleetPeers) verdictAt(branch string, key peerKey) (refused, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	refused, ok = p.judged[branch][key]
	return refused, ok
}

// record stores branch's verdict at key. A refusal is kept as the branch's
// last_refusal. An admission clears it when it is of the same tip, or when
// latest says this verdict is of the branch's current tip (the fetch judge);
// the merger's admission of an older local tip leaves a newer tip's refusal.
func (p *fleetPeers) record(branch string, key peerKey, reason string, latest bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.judged[branch]
	if m == nil {
		m = map[peerKey]bool{}
		p.judged[branch] = m
	}
	for k := range m {
		if k.main != key.main {
			delete(m, k)
		}
	}
	m[key] = reason != ""
	switch {
	case reason != "":
		r := PeerRefusal{Branch: branch, Tip: key.tip.String(), Reason: reason, At: time.Now().UTC()}
		p.refusals[branch] = r
		p.ring = append(p.ring, r)
		if over := len(p.ring) - refusalRing; over > 0 {
			p.ring = append(p.ring[:0:0], p.ring[over:]...)
		}
	case latest || p.refusals[branch].Tip == key.tip.String():
		delete(p.refusals, branch)
	}
}

// demote removes the local agent/<branch> after a refusal, if it is still at
// local (the head read before judging; zero = none). A branch that moved
// meanwhile is left for its own verdict.
func demote(ctx context.Context, svc *store.Service, branch, own string, local plumbing.Hash, lg *zerolog.Logger) {
	if local.IsZero() {
		return
	}
	dropped, err := svc.DemotePeerBranch(ctx, branch, own, local)
	switch {
	case err != nil:
		lg.Warn().Err(err).Str("branch", branch).Msg("fleet master: the refused branch could not be demoted; retried next round")
	case dropped:
		lg.Warn().Str("branch", branch).Str("tip", shortHash(local.String())).
			Msg("fleet master: an admitted branch is now refused; its local branch is demoted")
	}
}

// localHead is refs/heads/<branch>'s commit, zero when there is none.
func localHead(ctx context.Context, svc *store.Service, branch string) plumbing.Hash {
	h, err := svc.Branches().HeadCommit(ctx, branch)
	if err != nil || h == "" {
		return plumbing.ZeroHash
	}
	return plumbing.NewHash(h)
}

// lazyMain reads main's history once, on first use (PR #524 N2): a round
// where every branch's verdict is memoised reads none.
type lazyMain struct {
	svc  *store.Service
	hist *store.MainHistory
	err  error
	done bool
}

func (l *lazyMain) get(ctx context.Context) (*store.MainHistory, error) {
	if !l.done {
		l.done = true
		l.hist, l.err = l.svc.MainHistory(ctx)
	}
	return l.hist, l.err
}

// mayMerge is the merger's gate for one local agent/* branch of the fleet
// repository, on both hostings: the branch's verdict against the CURRENT
// main (memoised on (tip, main)). Refused, it is recorded and the local
// branch demoted; the merger skips it. main is the merger run's main tip.
func (p *fleetPeers) mayMerge(ctx context.Context, svc *store.Service, branch, own string, tip plumbing.Hash, main plumbing.Hash, hist *lazyMain) bool {
	if p == nil {
		return false
	}
	key := peerKey{tip: tip, main: main}
	if refused, ok := p.verdictAt(branch, key); ok {
		return !refused
	}
	lg := log.With().Str("branch", branch).Logger()
	h, err := hist.get(ctx)
	if err == nil && h.Tip() != main {
		err = fmt.Errorf("main moved during the merger's run")
	}
	if err != nil {
		lg.Warn().Err(err).Str("branch", branch).Msg("fleet master: branch not re-judged; not merged this run")
		return false
	}
	reason, err := svc.JudgePeerBranch(ctx, branch, own, tip, h)
	if err != nil {
		lg.Warn().Err(err).Str("branch", branch).Msg("fleet master: branch not re-judged; not merged this run")
		return false
	}
	p.record(branch, key, reason, false)
	if reason == "" {
		return true
	}
	lg.Warn().Str("branch", branch).Str("tip", shortHash(tip.String())).Str("reason", reason).
		Msg("fleet master: branch refused against the current main; left behind")
	demote(ctx, svc, branch, own, tip, &lg)
	return false
}

// isFleetMaster: ri is the fleet repository, mounted through the fleet door,
// and this instance is its master (Manager.isFleetMaster: the root key file
// is here and its certificate is the root on main). The one predicate for
// the merger and for the master's origin round.
func (ri *RepoInstance) isFleetMaster(svc *store.Service) bool {
	return ri.fleetDoor && ri.IsFleet() && ri.env.m.isFleetMaster(svc)
}

// publishesFleetMain: a push round of ri publishes main. ri is the fleet
// repository, mounted through the fleet door, and this instance holds its
// root, either as the master (the root on main) or before main ever held it
// (the root on the agent branch only: a forge create whose publish of main
// failed, #518). Every push path of the fleet repository asks this one
// predicate: the sync loop's round (fleetPeers.publish) and FleetRetry
// (Manager.pushFleet); fleetPushed only reads the outcome.
func (ri *RepoInstance) publishesFleetMain(svc *store.Service) bool {
	return ri.isFleetMaster(svc) || (ri.fleetDoor && ri.IsFleet() && ri.env.m.holdsFleetRootAt(svc, ri.AgentBranch()))
}

// admit judges every fetched member branch whose (tip, main) was not judged
// yet (step 2 above), against main's history read once for the round.
// Errors that left a branch unjudged are logged and retried on the next
// round.
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
	hist := &lazyMain{svc: svc}
	names := make([]string, 0, len(tips))
	for b := range tips {
		names = append(names, b)
	}
	sort.Strings(names)
	for _, branch := range names {
		if ctx.Err() != nil {
			return
		}
		key := peerKey{tip: tips[branch], main: plumbing.NewHash(mainTip)}
		if _, ok := p.verdictAt(branch, key); ok {
			continue
		}
		h, err := hist.get(ctx)
		if err != nil {
			lg.Warn().Err(err).Msg("fleet master: read main's history; member branches not judged this round")
			return
		}
		local := localHead(ctx, svc, branch)
		reason, err := svc.AdmitPeerBranch(ctx, branch, own, key.tip, h)
		if err != nil {
			lg.Warn().Err(err).Str("branch", branch).Msg("fleet master: member branch not judged; retried next round")
			continue
		}
		p.record(branch, key, reason, true)
		if reason != "" {
			lg.Warn().Str("branch", branch).Str("tip", shortHash(key.tip.String())).Str("reason", reason).
				Msg("fleet master: member branch refused; left behind")
			demote(ctx, svc, branch, own, local, lg)
		} else {
			lg.Info().Str("branch", branch).Str("tip", shortHash(key.tip.String())).
				Msg("fleet master: member branch admitted")
		}
	}
	// A branch gone from the origin (the fetch prunes within the peer
	// refspec) is forgotten, its refusal with it. Its local branch, if it was
	// admitted, stays for the merger, which re-judges it.
	p.mu.Lock()
	for b := range p.refusals {
		if _, ok := tips[b]; !ok {
			delete(p.judged, b)
			delete(p.refusals, b)
		}
	}
	p.mu.Unlock()
}

// publish is step 3: main = the agent branch, published without force
// (publishFleetMainOn, the call create and the registering arm make). It
// runs on EVERY round of a publishing instance whose own push did not fail,
// whether or not that push sent anything, so a main that create (or an
// earlier round) failed to publish is published by the next round. A
// refusal is recorded on the push status row, since PublishUpstream records
// nothing.
func (p *fleetPeers) publish(ctx context.Context, svc *store.Service, own string, auth transport.AuthMethod) (store.PushResult, error) {
	res, err := publishFleetMainOn(ctx, svc, own, auth)
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if rerr := svc.Remote().RecordPushError("origin", err.Error()); rerr != nil {
			return res, errors.Join(err, rerr)
		}
	}
	return res, err
}

// FleetPushRefusal is why ri accepts no push over /git, or "": a copy of the
// fleet repository that has an origin accepts none, master or member. Members
// push to the origin (the forge), never to another instance, and only the hub
// — the master's copy with NO origin — receives pushes (R-visibility: nobody
// pushes to a non-master). It is also what keeps the fetch-time judge sound:
// a master with an origin FETCHES every member branch, refused tips included,
// so it holds objects nobody admitted, and receive-pack judges only the
// commits a host does not hold. A clean merge of a refused tip and main
// pushed here would bring only the merge to the judge and launder the tip
// into main. An unreadable origin row refuses (fail closed).
func (m *Manager) FleetPushRefusal(ri *RepoInstance) string {
	if ri == nil || !ri.IsFleet() {
		return ""
	}
	o, err := m.originOf(ri)
	switch {
	case err != nil:
		return fmt.Sprintf("knomit: the origin of the fleet repository %q could not be read (%v); it accepts no push", ri.Name(), err)
	case o != nil:
		// The URL is not echoed: the pusher is a peer, and an operator's
		// origin URL may carry credentials.
		return fmt.Sprintf("knomit: this copy of the fleet repository %q has an origin; push your agent branch to the fleet's origin, not to this instance", ri.Name())
	}
	return ""
}

// FleetRefusals is the fleet master's last refusal of each member branch it
// left behind, by branch: refused at the fetch (a forge), or refused by the
// merger's re-judge against a main that moved (both hostings). Empty on a
// member and with no fleet repository mounted; a push receive-pack refuses
// is refused to the pusher's face instead. Held in memory: a restart judges
// again.
func (m *Manager) FleetRefusals() map[string]PeerRefusal {
	ri := m.fleetRepo()
	if ri == nil {
		return map[string]PeerRefusal{}
	}
	return ri.peers.Refusals()
}

// RefusalLog is a copy of the ring, newest first; nil-safe.
func (p *fleetPeers) RefusalLog() []PeerRefusal {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PeerRefusal, len(p.ring))
	for i, r := range p.ring {
		out[len(p.ring)-1-i] = r
	}
	return out
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
