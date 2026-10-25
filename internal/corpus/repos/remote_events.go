package repos

// Remote-health change publishing: the sync loop's half of the events.RemoteEvent
// stream (the event type and RemoteHealth live in repos/events).

import (
	"github.com/rs/zerolog"

	"knomit/internal/corpus/repos/events"
	"knomit/internal/engine/store"
)

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
	hub  *events.RepoEventHub
	name func() string
}

// remoteEdgeSink is this instance's sink: its Manager's hub and its live name.
func (ri *RepoInstance) remoteEdgeSink() remoteEdgeSink {
	return remoteEdgeSink{hub: ri.repoEventHub, name: ri.Name}
}

// publishRemoteEdge publishes a events.RemoteEvent when the origin's PERSISTED status
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
	s, p := events.RemoteHealth(after)
	if s == prevSync && p == prevPush {
		return
	}
	sink.hub.Publish(events.RemoteEvent{Repo: sink.name(), Sync: s, Push: p})
}
