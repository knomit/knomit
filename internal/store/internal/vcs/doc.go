// Package vcs is the store's git layer: Repo (formerly the store's
// repoHandler) owns the repository database handle, the go-git storer and
// repository, branches and their name-to-id cache, the per-branch write and
// push locks, commit signing and authoring (CommitSigs, the experiment parent
// check), commit trailers, the commit log and path-change derivation, history
// walks and the ontology at a commit.
//
// The commit path's shared primitives live here; the writers (fact writes,
// merges, reconcile, receive-pack) stay in the parent and use them in one
// order: LockBranch, CommitSigner and CommitSigs (which refuse a missing
// signer or an experiment with no nameable owner before any object is built),
// the object writes (WriteFileToStore, StoreCommit, ...), DeriveBeforeAdvance,
// the ref move, then NotifyCommit while the lock is still held. NotifyCommit is
// the single chokepoint after a ref moves: commit log, then the index
// manager's Sync, then experiment activity, then the onCommit observer. The
// parent wires the index manager and the observers in as values
// (SetIndexManager, SetOnCommit, SetOnDrop, SetConsensusBranchFunc); this
// package never calls up into the parent.
//
// It must not import its parent package store. The parent embeds *Repo in its
// repoHandler, so the sub-indices reach Repo's exported methods by promotion,
// and re-exports the public names callers outside store use.
package vcs
