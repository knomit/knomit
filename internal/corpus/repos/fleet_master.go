package repos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
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
	return ri.fleetDoor && ri.IsFleet() && m.holdsFleetRootAt(svc, ri.AgentBranch()) && !m.holdsFleetRootAt(svc, svc.Remote().UpstreamBranch())
}

// noteOwnJudge records a refusal of this instance's own commits by the judge
// that runs before the fleet's main moves (remotesync.ErrOwnCommitRefused: main
// was NOT moved) as the fleet's last error, so GET /fleet last_error shows it
// rather than a log line only; and, given nil (main moved), clears such a
// recorded refusal. Any other outcome is not its to record: the callers that
// own those errors record them.
func (m *Manager) noteOwnJudge(err error) {
	db := m.ControlDB()
	if db == nil {
		return
	}
	if errors.Is(err, remotesync.ErrOwnCommitRefused) {
		if rerr := recordFleetAttempt(db, err, time.Now()); rerr != nil {
			log.Warn().Err(rerr).Msg("fleet: recording the judge's refusal of this instance's own commit failed")
		}
		return
	}
	if err != nil {
		return
	}
	row, rerr := readFleetRow(db)
	if rerr == nil && strings.Contains(row.LastError, remotesync.OwnCommitRefusedText) {
		_ = recordFleetAttempt(db, nil, time.Now())
	}
}

// FleetDropResult is POST /fleet/drop-commit's answer.
type FleetDropResult struct {
	Dropped       string `json:"dropped"`
	Replayed      int    `json:"replayed"`
	MergesDropped int    `json:"merges_dropped"`
	// LeftOut: the commits the confirmed fallback left out (the merge and
	// every commit its side brought); empty for a plain drop.
	LeftOut []remotesync.LostCommit `json:"left_out,omitempty"`
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
// (remotesync.Index.DropUnpublishedCommit: that commit's own changes are gone, except
// where a later commit on the branch edited the same file; every other
// unpublished commit, members' merges included, is replayed), then publishes
// main. Only a non-merge commit main does not hold can be dropped (409
// not_unpublished, 422 merge_commit); one that would re-sign another agent's
// commits is refused (409 foreign_commit), so nothing but the named commit's
// own change is ever lost by a drop. Both of those refusals carry leave_out,
// what the confirmed fallback (LeaveOutFleetMerge) would lose instead.
func (m *Manager) DropFleetCommit(ctx context.Context, commit string) (FleetDropResult, error) {
	return m.dropFleetCommit(ctx, commit, false, nil)
}

// LeaveOutFleetMerge is POST /fleet/drop-commit with leave_out_merge: the
// confirmed fallback (#533 review 3 SF1) for when DropFleetCommit refuses
// with merge_commit or foreign_commit and the member who could fix the
// content cannot (it has left, or is gone for good). The merge that brought
// commit in (or commit itself, when it is that merge) is left out WHOLE,
// with every commit its side brought (remotesync.Index.DropUnpublishedCommitLeavingOutMerges);
// the rest of the branch is replayed and main is published. confirmed is the
// list of commit hashes the operator was shown (the refusal's leave_out) and
// confirmed: when what would be left out differs now, nothing changes (409
// leave_out_changed, with the new list).
func (m *Manager) LeaveOutFleetMerge(ctx context.Context, commit string, confirmed []string) (FleetDropResult, error) {
	if len(confirmed) == 0 {
		return FleetDropResult{}, fleetErr(http.StatusUnprocessableEntity, "leave_out_unconfirmed", "leaving a merge out needs the list of commits it loses, as the refusal listed them and the operator confirmed them")
	}
	return m.dropFleetCommit(ctx, commit, true, confirmed)
}

func (m *Manager) dropFleetCommit(ctx context.Context, commit string, leaveOut bool, confirmed []string) (FleetDropResult, error) {
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
		if leaveOut {
			out.Replayed, out.LeftOut, derr = svc.Remote().DropUnpublishedCommitLeavingOutMerges(ctx, ri.AgentBranch(), plumbing.NewHash(commit), confirmed)
			return
		}
		out.Replayed, out.MergesDropped, derr = svc.Remote().DropUnpublishedCommit(ctx, ri.AgentBranch(), plumbing.NewHash(commit), true)
	}); err != nil {
		return FleetDropResult{}, err
	}
	if errors.Is(derr, remotesync.ErrNotUnpublished) {
		return FleetDropResult{}, fleetErr(http.StatusConflict, "not_unpublished", "commit "+commit+" is not on this instance's fleet branch, or the fleet's main already holds it: there is nothing to drop")
	}
	var merge *remotesync.DropMergeError
	if errors.As(derr, &merge) {
		fe := fleetErr(http.StatusUnprocessableEntity, "merge_commit", "commit "+commit+" is a merge: it brings in another branch's commits and cannot be dropped as one; nothing was changed. Drop the commit that wrote the refused content, the one the Fleet page's notice names, or leave this whole merge out, which loses every commit listed with this answer")
		fe.LeaveOut = merge.LeaveOut
		return FleetDropResult{}, fe
	}
	var foreign *remotesync.DropForeignError
	if errors.As(derr, &foreign) {
		fe := fleetErr(http.StatusConflict, "foreign_commit", "commit "+commit+" came in with a merge of agent "+foreign.Agent+"'s branch, and dropping it would re-sign that agent's other commits, which the fleet refuses; nothing was changed. Ask "+foreign.Agent+" to fix the content on its branch, or, if it cannot (it has left, or is gone for good), leave the whole merge out, which loses every commit listed with this answer")
		fe.LeaveOut = foreign.LeaveOut
		return FleetDropResult{}, fe
	}
	var changed *remotesync.LeaveOutChangedError
	if errors.As(derr, &changed) {
		fe := fleetErr(http.StatusConflict, "leave_out_changed", "the fleet branch has moved since the list was shown: leaving the merge out would now lose the commits listed with this answer; nothing was changed")
		fe.LeaveOut = changed.LeaveOut
		return FleetDropResult{}, fe
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
