package repos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
//  2b. the master's duties (fleetPeers.duty): the leave duty revokes every
//     member whose `left` record is on main (fleet_leave.go), committed on
//     the agent branch so step 3 publishes it;
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
	p.record(branch, key, reason, refusedAgent(svc, reason, tip), false)
	if reason == "" {
		return true
	}
	lg.Warn().Str("branch", branch).Str("tip", shortHash(tip.String())).Str("reason", reason).
		Msg("fleet master: branch refused against the current main; left behind")
	demote(ctx, svc, branch, own, tip, &lg)
	return false
}

// FleetPausedNotHosting is fleetMasterPause's reason (GET /fleet
// master_paused): this master's copy follows another instance of the fleet.
const FleetPausedNotHosting = "not_hosting"

// fleetMasterPause is why this instance, a holder of the fleet's root,
// does no master work now and claims nothing, or "": FleetPausedNotHosting
// while its copy follows another instance of the fleet (a cert origin). In a
// self-hosted fleet the master is the host: a master that follows another
// instance is an old master that was replaced (it came back after the new
// master hosted with the dead-master confirm, and pointed at it) or a new
// master that has not hosted yet. A forge origin pauses nothing: a
// forge-hosted fleet's master works through the forge.
func (m *Manager) fleetMasterPause(ri *RepoInstance) string {
	if o, err := m.originOf(ri); err == nil && o != nil && o.AuthMethod == "cert" {
		return FleetPausedNotHosting
	}
	return ""
}

// actsAsFleetMaster: ri is the fleet repository and this instance is its
// master (isFleetMaster) and does master work now (no fleetMasterPause): the
// merger, the master's origin round, the hub's host-record round and the
// ontology refresh ask it. holdsFleetRoot says who holds the root.
func (ri *RepoInstance) actsAsFleetMaster(svc *store.Service) bool {
	return ri.isFleetMaster(svc) && ri.env.m.fleetMasterPause(ri) == ""
}

// refuseMasterPaused is masterFleet's refusal for a master that does no
// master work now.
func (m *Manager) refuseMasterPaused(ri *RepoInstance) error {
	if m.fleetMasterPause(ri) == FleetPausedNotHosting {
		return fleetErr(http.StatusConflict, "master_paused", "this instance holds the fleet's root key but its copy follows another instance of the fleet, which hosts it: it enrolls, revokes and merges nothing here. Act on the instance that hosts the fleet, or host the fleet here")
	}
	return nil
}

// fleetMasterClaimRound is a forge copy's claim to the master role in its
// origin round, right after the fetch (the hub's is in fleetHostRound): a
// root holder whose fresh main names no master (fleetCandidate) publishes
// the marker naming itself (claimFleetMaster). A main naming anyone is left
// alone. fleetOpMu is TryLocked like the hub's round.
func (ri *RepoInstance) fleetMasterClaimRound(ctx context.Context, svc *store.Service) {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() || svc == nil {
		return
	}
	m := ri.env.m
	if !m.fleetOpMu.TryLock() {
		return
	}
	defer m.fleetOpMu.Unlock()
	if !ri.fleetCandidate(svc) {
		return
	}
	m.claimFleetMaster(ctx, ri)
}

// isFleetMaster: ri is the fleet repository, mounted through the fleet door,
// and this instance is its master (Manager.isFleetMaster: it holds the root
// and the master marker on main names it).
func (ri *RepoInstance) isFleetMaster(svc *store.Service) bool {
	return ri.fleetDoor && ri.IsFleet() && ri.env.m.isFleetMaster(svc)
}

// publishesFleetMain: a push round of ri publishes main. ri is the fleet
// repository, mounted through the fleet door, and this instance is its
// master (isFleetMaster: the root, and main names it), or holds the root
// before main ever held it (the root and the marker on the agent branch
// only: a forge create whose publish of main failed, #518). A root holder
// that main does not name publishes nothing, nor does one whose copy follows
// another instance (fleetMasterPause). Every push path of the fleet
// repository asks this one predicate: the sync loop's round
// (fleetPeers.publish) and FleetRetry (Manager.pushFleet); fleetPushed only
// reads the outcome.
func (ri *RepoInstance) publishesFleetMain(svc *store.Service) bool {
	if ri.env.m.fleetMasterPause(ri) != "" {
		return false
	}
	if ri.isFleetMaster(svc) {
		return true
	}
	// A forge create whose publish failed (#518): the root, and the marker,
	// are on the agent branch only; main has never held the root.
	m := ri.env.m
	return ri.fleetDoor && ri.IsFleet() && m.holdsFleetRootAt(svc, ri.AgentBranch()) && !m.holdsFleetRootAt(svc, svc.UpstreamBranch())
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
func (p *fleetPeers) publish(ctx context.Context, svc *store.Service, own string, auth transport.AuthMethod) (store.PushResult, error) {
	res, err := publishFleetMainOn(ctx, svc, own, auth)
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if rerr := svc.Remote().RecordPushError("origin", err.Error()); rerr != nil {
			return res, errors.Join(err, rerr)
		}
	}
	return res, err
}

// noteOwnJudge records a refusal of this instance's own commits by the judge
// that runs before the fleet's main moves (store.ErrOwnCommitRefused: main
// was NOT moved) as the fleet's last error, so GET /fleet last_error shows it
// rather than a log line only; and, given nil (main moved), clears such a
// recorded refusal. Any other outcome is not its to record: the callers that
// own those errors record them.
func (m *Manager) noteOwnJudge(err error) {
	db := m.ControlDB()
	if db == nil {
		return
	}
	if errors.Is(err, store.ErrOwnCommitRefused) {
		if rerr := recordFleetAttempt(db, err, time.Now()); rerr != nil {
			log.Warn().Err(rerr).Msg("fleet: recording the judge's refusal of this instance's own commit failed")
		}
		return
	}
	if err != nil {
		return
	}
	row, rerr := readFleetRow(db)
	if rerr == nil && strings.Contains(row.LastError, store.OwnCommitRefusedText) {
		_ = recordFleetAttempt(db, nil, time.Now())
	}
}

// mainHeldCommit is the commit a recorded own-judge refusal names
// ("knomit: main was not moved: commit <hash> refused by ..."), or "".
func mainHeldCommit(lastError string) string {
	_, rest, ok := strings.Cut(lastError, store.OwnCommitRefusedText+" ")
	if !ok {
		return ""
	}
	h, _, _ := strings.Cut(rest, " ")
	if len(h) != 40 || strings.Trim(h, "0123456789abcdef") != "" {
		return ""
	}
	return h
}

// FleetDropResult is POST /fleet/drop-commit's answer.
type FleetDropResult struct {
	Dropped       string `json:"dropped"`
	Replayed      int    `json:"replayed"`
	MergesDropped int    `json:"merges_dropped"`
	// Published: main moved (or already was) at the rebuilt branch.
	// PublishError: why not, when it did not (another refused commit, a
	// forge that refused the push); the fleet's last_error says it too.
	Published    bool   `json:"published"`
	PublishError string `json:"publish_error,omitempty"`
}

// DropFleetCommit is POST /fleet/drop-commit: the operator's way out when the
// judge of this instance's own commits holds the fleet's main back
// (GET /fleet last_error_main_held). The refused commit stays on the agent
// branch and every later move of main is refused with it; this rebuilds the
// fleet repository's agent branch on main without that commit
// (store.DropUnpublishedCommit: that commit's own changes are gone, except
// where a later commit on the branch edited the same file; every other
// unpublished commit, members' merges included, is replayed), then publishes
// main. Only a commit main does not hold can be dropped (409
// not_unpublished).
func (m *Manager) DropFleetCommit(ctx context.Context, commit string) (FleetDropResult, error) {
	defer m.lockFleetOp()()
	ri := m.fleetRepo()
	if ri == nil {
		return FleetDropResult{}, fleetErr(http.StatusConflict, "not_registered", "this instance is in no fleet")
	}
	if len(commit) != 40 || strings.Trim(commit, "0123456789abcdef") != "" {
		return FleetDropResult{}, fleetErr(http.StatusUnprocessableEntity, "invalid_commit", "commit must be a full 40-hex commit hash")
	}
	out := FleetDropResult{Dropped: commit}
	var derr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			derr = errors.New("fleet repository is not open")
			return
		}
		out.Replayed, out.MergesDropped, derr = svc.DropUnpublishedCommit(ctx, ri.AgentBranch(), plumbing.NewHash(commit))
	}); err != nil {
		return FleetDropResult{}, err
	}
	if errors.Is(derr, store.ErrNotUnpublished) {
		return FleetDropResult{}, fleetErr(http.StatusConflict, "not_unpublished", "commit "+commit+" is not on this instance's fleet branch, or the fleet's main already holds it: there is nothing to drop")
	}
	if derr != nil {
		return FleetDropResult{}, derr
	}
	ri.consensusKick()
	if err := m.publishFleetMainNow(ctx, ri); err != nil {
		out.PublishError = credentialFreeText(err.Error())
	} else {
		out.Published = true
	}
	return out, nil
}

// fleetMainAdvanced is the local reconcile's after hook (the hub's main
// following its agent branch): on the fleet repository it records the
// advance's outcome with noteOwnJudge. Every other repository: nothing.
func (ri *RepoInstance) fleetMainAdvanced(err error) {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() {
		return
	}
	ri.env.m.noteOwnJudge(err)
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
		return fmt.Sprintf("knomit: this copy of the fleet repository %q %s, not to this instance", ri.Name(), FleetPushRefusalHasOrigin)
	}
	// The hub takes pushes only while this instance is the fleet's master:
	// once it stood down nothing merges them, and the new master hosts the
	// fleet with what main held at the stand-down.
	master := false
	if err := ri.WithRead(func(svc *store.Service) { master = ri.isFleetMaster(svc) }); err != nil || !master {
		return fmt.Sprintf("knomit: this copy of the fleet repository %q: %s; point this copy at the new host (Fleet → Settings → Point at the new host) once one hosts the fleet", ri.Name(), FleetPushRefusalStoodDown)
	}
	return ""
}

// FleetPushRefusalStoodDown is the part of the hub's refusal, once its
// master stood down, that every refused pusher records as its sync error:
// GET /fleet's last_error_stood_down reads it there.
const FleetPushRefusalStoodDown = "its master stood down, and it takes no push"

// FleetPushRefusalHasOrigin is the part of FleetPushRefusal's text for a
// copy with an origin that every refused pusher records as its sync error:
// GET /fleet's last_error_fleet_moved reads it there.
const FleetPushRefusalHasOrigin = "has an origin; push your agent branch to the fleet's origin"

// FleetPushJudged is the receive-pack verdict callback for ri
// (store.PushPolicy.Judged): on the fleet repository, the copy that takes
// pushes is the hub (FleetPushRefusal refuses every other copy before the
// judge), the master's, and receive-pack IS its judge, so its verdicts go
// where the fetch judge's do (fleetPeers.pushed): a refusal is the branch's
// last_refusal and an entry of the ring GET /fleet/activity shows. nil for
// any other repository.
func (m *Manager) FleetPushJudged(ri *RepoInstance) func(branch, tip, agent, refusal string) {
	if ri == nil || !ri.IsFleet() || ri.peers == nil {
		return nil
	}
	return func(branch, tip, agent, refusal string) { ri.peers.pushed(branch, tip, refusal, agent) }
}

// FleetRefusals is the fleet master's last refusal of each member branch it
// left behind, by branch: refused at the fetch (a forge), by the merger's
// re-judge against a main that moved (both hostings), or by the hub's
// receive-pack (to the pusher's face, and kept here). Empty on a member and
// with no fleet repository mounted. Held in memory: a restart judges again.
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
