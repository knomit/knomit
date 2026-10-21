// Package remotesync is the store's origin-synchronisation cluster: the origin
// remote and its status row (Index: GetRemote, DeleteRemote, the sync and
// push status), the fetch and Sync (the reconcile of the consensus branch,
// then of the agent branch), Push, PublishUpstream and PublishTo, the rewind
// replay with its watermark (remote_reconcile*.go), the origin identity
// that decides whether a round switched origins, peer admission for the fleet
// master's fetch, the publish-time judge of this instance's own commits and
// the unpublished-commit drop (verify_own.go, drop.go).
//
// It sits above vcs, verify and merge in the store's child order. Repo wraps
// the merge handle (which wraps the one shared vcs handle), so the methods
// that used to be declared on the parent's repoHandler stay reachable as
// rh.<Name> by promotion; the parent embeds it next to the other children.
// Embedding is not virtual: nothing here calls a method declared on the
// parent. The consensus-branch record and the bodies of the Service's peer
// admission and local-upstream methods live here (consensus_branch.go,
// peer_admit.go, local_reconcile.go) and the remote bootstrap (bootstrap.go:
// CloneFrom, InitFromRemote, InitSubscription); replay.go, the thin Service
// methods (service_sync.go, consensus_branch.go, repo.go) and the local
// OpenRepo/InitRepo in repo.go stay in the parent and call the exported
// cores; the receive-pack handler is the gitserve child, which calls
// AdvanceBranchTo.
//
// The package is named remotesync, not sync, so it does not collide with the
// standard library's sync (the same collision that ruled out runtime for
// another child). The order inside Index.Sync is load-bearing: the
// remote-tracking ref of the consensus branch is read under the config read
// lock BEFORE the fetch moves it, the fetch runs, a missing watermark is
// seeded from the pre-fetch position, and only then does the reconcile run.
package remotesync
