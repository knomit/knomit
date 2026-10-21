package vcs

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
	"knomit/internal/store/model"
)

// Accessors and build-time setters for the Repo's wiring. The parent
// store reads the shared backend state through the getters and wires the
// hooks through the setters; every setter is called at build/swap time
// (Service.Open, Set* on the Service) before the store is published, exactly
// as the field assignments it replaces were, so none takes a lock.

// DB is the repository database.
func (rh *Repo) DB() *sql.DB { return rh.db }

// Storer is the go-git storage over the repository database.
func (rh *Repo) Storer() *storegit.Storer { return rh.gits }

// Repository is the go-git repository (nil until OpenRepo/InitRepo/Clone).
func (rh *Repo) Repository() *gogit.Repository { return rh.repo }

// SetRepository installs the go-git repository opened over Storer.
func (rh *Repo) SetRepository(r *gogit.Repository) { rh.repo = r }

// Name is the repo name, derived from the database path at Open.
func (rh *Repo) Name() string { return rh.name }

// SetName sets the repo name.
func (rh *Repo) SetName(name string) { rh.name = name }

// SetOnDrop installs the hook DropBranch runs after a branch is dropped.
func (rh *Repo) SetOnDrop(f func(context.Context) error) { rh.onDrop = f }

// SetOnDropBranchRows installs the delete DropBranchLocked runs for a branch
// being dropped, after its branch_facts rows and before its branches row: the
// rows other clusters keep per branch in tables that reference branches(id)
// without a cascade. It runs with DropBranchLocked's ctx (inside the caller's
// transaction, if any) and under the branch's write lock, and its error is
// returned as DropBranchLocked's. The parent registers it when it builds the
// Repo; a Repo without one refuses every drop (ErrNoDropBranchRowsHook)
// before removing anything.
func (rh *Repo) SetOnDropBranchRows(f func(ctx context.Context, branchID int64) error) {
	rh.onDropBranchRows = f
}

// SetIndexManager installs the index manager NotifyCommit syncs after every
// commit (nil: NotifyCommit skips the sync).
func (rh *Repo) SetIndexManager(im model.IndexManager) { rh.im = im }

// SetOnCommit installs the external commit observer NotifyCommit calls last.
func (rh *Repo) SetOnCommit(f func(branch, hash string)) { rh.onCommit = f }

// ConsensusBranchFunc returns the consensus-branch resolver (nil only on a
// Repo built bare in a test).
func (rh *Repo) ConsensusBranchFunc() func() string { return rh.consensusBranch }

// SetConsensusBranchFunc installs the consensus-branch resolver.
func (rh *Repo) SetConsensusBranchFunc(f func() string) { rh.consensusBranch = f }

// NetTimeout bounds every remote git network operation (0: no bound).
func (rh *Repo) NetTimeout() time.Duration { return rh.netTimeout }

// SetNetTimeout sets NetTimeout.
func (rh *Repo) SetNetTimeout(d time.Duration) { rh.netTimeout = d }

// FactRoot is the configured ontology root, empty when not configured (see
// OntologyRoot for the defaulted value).
func (rh *Repo) FactRoot() string { return rh.factRoot }

// SetFactRoot sets the configured ontology root.
func (rh *Repo) SetFactRoot(root string) { rh.factRoot = root }

// ReadOnly reports whether authored commits are refused (a subscription).
func (rh *Repo) ReadOnly() bool { return rh.readOnly }

// SetReadOnly sets ReadOnly.
func (rh *Repo) SetReadOnly(ro bool) { rh.readOnly = ro }

// SetSigner sets the commit signer.
func (rh *Repo) SetSigner(s ssh.Signer) { rh.signer = s }

// SetAgentID sets this instance's persisted agent id.
func (rh *Repo) SetAgentID(id string) { rh.agentID = id }

// AcceptList is the operator's accept list (nil is empty).
func (rh *Repo) AcceptList() model.AcceptList { return rh.acceptList }

// SetAcceptList sets AcceptList.
func (rh *Repo) SetAcceptList(a model.AcceptList) { rh.acceptList = a }

// OwnKeys returns every key this instance's fleet member record has held, or
// nil when no source is installed (standalone).
func (rh *Repo) OwnKeys() []ssh.PublicKey {
	if rh.ownKeys == nil {
		return nil
	}
	return rh.ownKeys()
}

// SetOwnKeys installs the source of OwnKeys.
func (rh *Repo) SetOwnKeys(f func() []ssh.PublicKey) { rh.ownKeys = f }

// ReplaySkip is the set of paths a rewind replay never carries.
func (rh *Repo) ReplaySkip() map[string]bool { return rh.replaySkip }

// SetReplaySkip sets ReplaySkip.
func (rh *Repo) SetReplaySkip(set map[string]bool) { rh.replaySkip = set }

// RLockConfig takes the remote-config read lock (ConfigureRemote holds the
// write side across a refspec rewrite) and returns its unlock.
func (rh *Repo) RLockConfig() func() {
	rh.configMu.RLock()
	return rh.configMu.RUnlock
}

// OntologyCache is the cache of the ontology validateWrite last parsed.
func (rh *Repo) OntologyCache() *OntCache { return &rh.writeOnt }

// InitRepoConfig stamps the repo config with the knomit identity and disables
// GPG signing, which must be done for both fresh and remote-initialized repos.
func InitRepoConfig(repo *gogit.Repository, label string) error {
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("%s: read config: %w", label, err)
	}
	cfg.User.Name = "knomit"
	cfg.User.Email = "knomit@local"
	if cfg.Raw != nil {
		cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	}
	if err := repo.SetConfig(cfg); err != nil {
		return fmt.Errorf("%s: set config: %w", label, err)
	}
	return nil
}

// DefaultAgentBranch returns "agent/<hostname>", falling back to "agent/local".
func DefaultAgentBranch() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "local"
	}
	return "agent/" + hostname
}
