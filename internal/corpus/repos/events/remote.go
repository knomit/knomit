package events

// Remote-health change events.
//
// The repo list shows a health dot for EVERY repo, not just the open one, and
// the per-branch stream that carries sync_*/push_* is held for the active repo
// only. So a repo's remote health is a repo-level fact (see the membership test
// on handleRepoEvents): the persisted status of its origin, as an entry in the
// list. These events keep a listed row current without a poll per repo.

import "knomit/internal/engine/store/remotesync"

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
