package store

import (
	"context"
	"database/sql"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
)

// Accessors and build-time setters for the repoHandler's wiring. The parent
// store reads the shared backend state through the getters and wires the
// hooks through the setters; every setter is called at build/swap time
// (Service.Open, Set* on the Service) before the store is published, exactly
// as the field assignments it replaces were, so none takes a lock.

// DB is the repository database.
func (rh *repoHandler) DB() *sql.DB { return rh.db }

// Storer is the go-git storage over the repository database.
func (rh *repoHandler) Storer() *storegit.Storer { return rh.gits }

// Repository is the go-git repository (nil until OpenRepo/InitRepo/Clone).
func (rh *repoHandler) Repository() *gogit.Repository { return rh.repo }

// SetRepository installs the go-git repository opened over Storer.
func (rh *repoHandler) SetRepository(r *gogit.Repository) { rh.repo = r }

// Name is the repo name, derived from the database path at Open.
func (rh *repoHandler) Name() string { return rh.name }

// SetName sets the repo name.
func (rh *repoHandler) SetName(name string) { rh.name = name }

// SetOnDrop installs the hook DropBranch runs after a branch is dropped.
func (rh *repoHandler) SetOnDrop(f func(context.Context) error) { rh.onDrop = f }

// SetIndexManager installs the index manager notifyCommit syncs after every
// commit (nil: notifyCommit skips the sync).
func (rh *repoHandler) SetIndexManager(im IndexManager) { rh.im = im }

// SetOnCommit installs the external commit observer notifyCommit calls last.
func (rh *repoHandler) SetOnCommit(f func(branch, hash string)) { rh.onCommit = f }

// ConsensusBranchFunc returns the consensus-branch resolver (nil only on a
// repoHandler built bare in a test).
func (rh *repoHandler) ConsensusBranchFunc() func() string { return rh.consensusBranch }

// SetConsensusBranchFunc installs the consensus-branch resolver.
func (rh *repoHandler) SetConsensusBranchFunc(f func() string) { rh.consensusBranch = f }

// NetTimeout bounds every remote git network operation (0: no bound).
func (rh *repoHandler) NetTimeout() time.Duration { return rh.netTimeout }

// SetNetTimeout sets NetTimeout.
func (rh *repoHandler) SetNetTimeout(d time.Duration) { rh.netTimeout = d }

// FactRoot is the configured ontology root, empty when not configured (see
// ontologyRoot for the defaulted value).
func (rh *repoHandler) FactRoot() string { return rh.factRoot }

// SetFactRoot sets the configured ontology root.
func (rh *repoHandler) SetFactRoot(root string) { rh.factRoot = root }

// ReadOnly reports whether authored commits are refused (a subscription).
func (rh *repoHandler) ReadOnly() bool { return rh.readOnly }

// SetReadOnly sets ReadOnly.
func (rh *repoHandler) SetReadOnly(ro bool) { rh.readOnly = ro }

// SetSigner sets the commit signer.
func (rh *repoHandler) SetSigner(s ssh.Signer) { rh.signer = s }

// SetAgentID sets this instance's persisted agent id.
func (rh *repoHandler) SetAgentID(id string) { rh.agentID = id }

// AcceptList is the operator's accept list (nil is empty).
func (rh *repoHandler) AcceptList() AcceptList { return rh.acceptList }

// SetAcceptList sets AcceptList.
func (rh *repoHandler) SetAcceptList(a AcceptList) { rh.acceptList = a }

// OwnKeys returns every key this instance's fleet member record has held, or
// nil when no source is installed (standalone).
func (rh *repoHandler) OwnKeys() []ssh.PublicKey {
	if rh.ownKeys == nil {
		return nil
	}
	return rh.ownKeys()
}

// SetOwnKeys installs the source of OwnKeys.
func (rh *repoHandler) SetOwnKeys(f func() []ssh.PublicKey) { rh.ownKeys = f }

// ReplaySkip is the set of paths a rewind replay never carries.
func (rh *repoHandler) ReplaySkip() map[string]bool { return rh.replaySkip }

// SetReplaySkip sets ReplaySkip.
func (rh *repoHandler) SetReplaySkip(set map[string]bool) { rh.replaySkip = set }

// RLockConfig takes the remote-config read lock (configureRemote holds the
// write side across a refspec rewrite) and returns its unlock.
func (rh *repoHandler) RLockConfig() func() {
	rh.configMu.RLock()
	return rh.configMu.RUnlock
}

// OntologyCache is the cache of the ontology validateWrite last parsed.
func (rh *repoHandler) OntologyCache() *OntCache { return &rh.writeOnt }
