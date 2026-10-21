package repos

// Remote-health change events.
//
// The repo list shows a health dot for EVERY repo, not just the open one, and
// the per-branch stream that carries sync_*/push_* is held for the active repo
// only. So a repo's remote health is a repo-level fact (see the membership test
// on handleRepoEvents): the persisted status of its origin, as an entry in the
// list. These events keep a listed row current without a poll per repo.

import (
	"github.com/rs/zerolog"

	"knomit/internal/store"
	"knomit/internal/store/remotesync"
)

// RemoteEvent reports a repo's remote health: the two persisted status columns
// of its origin, in the repos list's own vocabulary (`ok | error | ""`, where
// "" means that half has never run), so a consumer replaces the row's `remote`
// field with the event as it stands. Both halves ride on every event, although
// an edge is usually on one of them: the payload is the row's new value, not a
// delta, so a client never has to merge.
type RemoteEvent struct {
	Repo string `json:"repo"`
	Sync string `json:"sync"`
	Push string `json:"push"`
}

// RemoteStatusValue renders one persisted status column (last_status /
// last_push_status) in the wire vocabulary: NULL (never ran) is "".
func RemoteStatusValue(status *string) string {
	if status == nil {
		return ""
	}
	return *status
}

// RemoteHealth is the (sync, push) pair of a remote record, in the wire
// vocabulary. GET /repos and RemoteEvent both read it, so the listing and the
// stream cannot spell the same row two ways.
func RemoteHealth(r *remotesync.Remote) (sync, push string) {
	return RemoteStatusValue(r.LastStatus), RemoteStatusValue(r.LastPushStatus)
}

// remoteEdgeSink is where a reconcile loop publishes its RemoteEvents: the
// server-wide hub, plus the repo's name READ AT PUBLISH TIME. The name is a
// func, not a string, because RenameRepo re-keys the instance without
// restarting its Sync stage: a name captured when the loop started would stamp
// every later edge with the OLD name, so the renamed row's dot would never
// move and a repo later created under the old name would receive these edges.
// publishIndex reads m.r.Name() per event for the same reason.
//
// The zero value publishes nothing (a loop built without a hub, in tests).
type remoteEdgeSink struct {
	hub  *RepoEventHub
	name func() string
}

// remoteEdgeSink is this instance's sink: its Manager's hub and its live name.
func (ri *RepoInstance) remoteEdgeSink() remoteEdgeSink {
	return remoteEdgeSink{hub: ri.repoEventHub, name: ri.Name}
}

// publishRemoteEdge publishes a RemoteEvent when the origin's PERSISTED status
// differs from the snapshot the tick took before it ran, and says nothing
// otherwise. EDGES ONLY: a steady-state tick, healthy or failing, is silent.
//
// Both sides of the comparison are the persisted row: the snapshot is
// fresh.LastStatus/LastPushStatus from the top of the tick, the "after" is a
// re-read here. Never the loop's in-memory failure counters: those start at
// zero after a server restart while the row still says "error", and the first
// healthy tick after that restart is exactly the recovery edge a listed row's
// dot needs (the rule shouldBroadcastSyncOK follows for the per-branch
// stream). Re-reading rather than predicting from the step's result also keeps
// the event and GET /repos agreeing by construction: a step that wrote nothing
// (a refused push hook, an abandoned tick) changes nothing here either.
//
// A failed or empty re-read publishes nothing: the client's list refetch on a
// reconnect's `ready` is the fallback, as it is for every repo-event.
func publishRemoteEdge(sink remoteEdgeSink, svc *store.Service, prevSync, prevPush string, lg *zerolog.Logger) {
	if sink.hub == nil || sink.name == nil {
		return
	}
	after, err := svc.Remote().GetRemote("origin")
	if err != nil {
		lg.Debug().Err(err).Msg("reconcile: remote re-read for the health edge failed; no event")
		return
	}
	if after == nil {
		return
	}
	s, p := RemoteHealth(after)
	if s == prevSync && p == prevPush {
		return
	}
	sink.hub.publish(RemoteEvent{Repo: sink.name(), Sync: s, Push: p})
}
