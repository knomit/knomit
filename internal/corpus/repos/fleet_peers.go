package repos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
)

// fleetPeers is the sync-to-fleet seam: the master's judging state for one
// fleet repository (the admission memo, the refusal ring) and the func values
// the sync loop calls for the fleet duties (master, publishes, claim, duty,
// tick, halt, fetchRefused), wired in manager.go. The R-merge design note is
// at the top of fleet_master.go.

// PeerRefusal is the fleet master's refusal of one member branch: one it
// fetched (left behind, not retried until the branch or main moves), one its
// merger re-judged, or one pushed to the hub (refused to the pusher's face).
// Agent is the agent id of the refused tip's author (store.CommitAgent, the
// address the judge reads), "" when it is not an agent address: a refused
// first join has no record on main, so this is what ties the refusal to the
// enrolled id.
type PeerRefusal struct {
	Branch string    `json:"branch"`
	Tip    string    `json:"tip"`
	Agent  string    `json:"agent,omitempty"`
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
	// duty is the master's duties in its origin round (a forge), after the
	// fetch judge and before the push and the publish of main, so what it
	// commits goes out in the same round (RepoInstance.fleetMasterDuty: the
	// leave duty). nil: none.
	duty func(context.Context, *store.Service)
	// tick is every copy's tick duty in its origin round, master or member,
	// after a successful fetch and before the master's duties
	// (RepoInstance.fleetTickRound: main's newer CRL installed here). nil:
	// none.
	tick func(context.Context, *store.Service)
	// halt says why this copy's origin rounds stop, "" while they run
	// (Manager.fleetSyncHalt: this member's certificate is revoked). nil:
	// never.
	halt func() string
	// fetchRefused is told a failed fetch of this copy and answers whether
	// it was a TLS refusal (RepoInstance.fleetFetchRefused: recorded as the
	// fleet's last error, the round then pushes nothing). nil: never.
	fetchRefused func(error) bool

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
	// claim is a candidate's claim to the master role in its origin round
	// (RepoInstance.fleetMasterClaimRound); nil-safe.
	claim func(context.Context, *store.Service)
}

// claimMaster runs the master marker claim of this round; no-op on nil.
func (p *fleetPeers) claimMaster(ctx context.Context, svc *store.Service) {
	if p != nil && p.claim != nil {
		p.claim(ctx, svc)
	}
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

// runDuty runs the master's round duties (duty); nil-safe.
func (p *fleetPeers) runDuty(ctx context.Context, svc *store.Service) {
	if p != nil && p.duty != nil {
		p.duty(ctx, svc)
	}
}

// halted is why this copy's origin rounds stop (halt), "" while they run;
// nil-safe.
func (p *fleetPeers) halted() string {
	if p == nil || p.halt == nil {
		return ""
	}
	return p.halt()
}

// refusedFetch reports a failed fetch (fetchRefused): true when it was a TLS
// refusal and the round must push nothing; nil-safe.
func (p *fleetPeers) refusedFetch(err error) bool {
	return p != nil && p.fetchRefused != nil && p.fetchRefused(err)
}

// runTick runs every copy's tick duty (tick); nil-safe.
func (p *fleetPeers) runTick(ctx context.Context, svc *store.Service) {
	if p != nil && p.tick != nil {
		p.tick(ctx, svc)
	}
}

// verdictAt is the memoised verdict of branch at key, if it was judged there.
func (p *fleetPeers) verdictAt(branch string, key peerKey) (refused, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	refused, ok = p.judged[branch][key]
	return refused, ok
}

// record stores branch's verdict at key (agent: the tip's author's id). A
// refusal is kept as the branch's last_refusal. An admission clears it when
// it is of the same tip, or when latest says this verdict is of the branch's
// current tip (the fetch judge); the merger's admission of an older local tip
// leaves a newer tip's refusal.
func (p *fleetPeers) record(branch string, key peerKey, reason, agent string, latest bool) {
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
	p.verdictLocked(branch, key.tip.String(), reason, agent, latest)
}

// pushed records receive-pack's verdict on a push of branch at tip, on the
// hub (Manager.FleetPushJudged): a refusal is the branch's last_refusal and
// goes into the ring like the fetch judge's; an admission is of the branch's
// new tip, so it clears the branch's refusal. The memo is not touched: the
// merger re-judges an admitted tip against the main it merges into.
func (p *fleetPeers) pushed(branch, tip, reason, agent string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verdictLocked(branch, tip, reason, agent, true)
}

// verdictLocked is the refusal bookkeeping record and pushed share, under
// p.mu. The same branch refused again at the same tip for the same reason
// (main moved, or the same push was retried) is not a new refusal: the
// last_refusal and the ring keep the first one (PR #527 N3), so one stuck
// branch cannot fill the ring.
func (p *fleetPeers) verdictLocked(branch, tip, reason, agent string, latest bool) {
	cur, refused := p.refusals[branch]
	switch {
	case reason != "" && refused && cur.Tip == tip && cur.Reason == reason:
	case reason != "":
		r := PeerRefusal{Branch: branch, Tip: tip, Agent: agent, Reason: reason, At: time.Now().UTC()}
		p.refusals[branch] = r
		p.ring = append(p.ring, r)
		if over := len(p.ring) - refusalRing; over > 0 {
			p.ring = append(p.ring[:0:0], p.ring[over:]...)
		}
	case latest || cur.Tip == tip:
		delete(p.refusals, branch)
	}
}

// refusedAgent is the agent id of a refused tip's author; "" for an
// admission, which reads nothing.
func refusedAgent(svc *store.Service, reason string, tip plumbing.Hash) string {
	if reason == "" {
		return ""
	}
	return svc.CommitAgent(tip)
}

// demote removes the local agent/<branch> after a refusal, if it is still at
// local (the head read before judging; zero = none). A branch that moved
// meanwhile is left for its own verdict.
func demote(ctx context.Context, svc *store.Service, branch, own string, local plumbing.Hash, lg *zerolog.Logger) {
	if local.IsZero() {
		return
	}
	dropped, err := svc.Remote().DemotePeerBranch(ctx, branch, own, local)
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
	h, err := svc.HeadCommit(ctx, branch)
	if err != nil || h == "" {
		return plumbing.ZeroHash
	}
	return plumbing.NewHash(h)
}

// lazyMain reads main's history once, on first use (PR #524 N2): a round
// where every branch's verdict is memoised reads none.
type lazyMain struct {
	svc  *store.Service
	hist *remotesync.MainHistory
	err  error
	done bool
}

func (l *lazyMain) get(ctx context.Context) (*remotesync.MainHistory, error) {
	if !l.done {
		l.done = true
		l.hist, l.err = l.svc.Remote().MainHistory(ctx)
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
	reason, err := svc.Remote().JudgePeerBranch(ctx, branch, own, tip, h)
	if err != nil {
		lg.Warn().Err(err).Str("branch", branch).Msg("fleet master: branch not re-judged; not merged this run")
		return false
	}
	p.record(branch, key, reason, refusedAgent(svc, reason, tip), false)
	if reason == "" {
		return true
	}
	lg.Warn().Str("branch", branch).Str("tip", shortHash(tip.String())).Str("reason", reason).
		Msg("fleet master: branch refused against the current main; left behind")
	demote(ctx, svc, branch, own, tip, &lg)
	return false
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
	mainTip, err := svc.HeadCommit(ctx, svc.Remote().UpstreamBranch())
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
		reason, err := svc.Remote().AdmitPeerBranch(ctx, branch, own, key.tip, h)
		if err != nil {
			lg.Warn().Err(err).Str("branch", branch).Msg("fleet master: member branch not judged; retried next round")
			continue
		}
		p.record(branch, key, reason, refusedAgent(svc, reason, key.tip), true)
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
func (p *fleetPeers) publish(ctx context.Context, svc *store.Service, own string, auth transport.AuthMethod) (remotesync.PushResult, error) {
	res, err := publishFleetMainOn(ctx, svc, own, auth)
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if rerr := svc.Remote().RecordPushError("origin", err.Error()); rerr != nil {
			return res, errors.Join(err, rerr)
		}
	}
	return res, err
}

// mainHeldCommit is the commit a recorded own-judge refusal names
// ("knomit: main was not moved: commit <hash> refused by ..."), or "".
func mainHeldCommit(lastError string) string {
	_, rest, ok := strings.Cut(lastError, remotesync.OwnCommitRefusedText+" ")
	if !ok {
		return ""
	}
	h, _, _ := strings.Cut(rest, " ")
	if len(h) != 40 || strings.Trim(h, "0123456789abcdef") != "" {
		return ""
	}
	return h
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
