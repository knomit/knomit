package vcs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/sqlite"
	"knomit/internal/engine/store/model"
)

// branchCache is a simple thread-safe name→ID cache.
type branchCache struct {
	mu     sync.RWMutex
	byName map[string]int64
}

func newBranchCache() *branchCache {
	return &branchCache{byName: make(map[string]int64)}
}

func (c *branchCache) get(name string) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.byName[name]
	return id, ok
}

func (c *branchCache) set(name string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byName[name] = id
}

func (c *branchCache) remove(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byName, name)
}

// Repo owns the database handle, branch cache, and branch operations.
// It is also the home for shared git-level plumbing that used to be scattered
// across facts.Index / index.Index: the SSH commit signer, the onCommit
// observer, the author/committer signature helpers, and the commit-log
// maintenance methods. Consumers reach UP to Repo for these — they
// never reach sideways through a sibling subsystem.
type Repo struct {
	db     *sql.DB
	cache  *branchCache
	onDrop func(context.Context) error
	gits   *storegit.Storer
	repo   *gogit.Repository          // nil until OpenRepo/InitRepo/Clone called
	signer atomic.Pointer[ssh.Signer] // SSH signer for commit signing (shared); nil pointer = none

	// onDropBranchRows deletes the per-branch rows higher clusters keep in
	// tables without an ON DELETE CASCADE; DropBranchLocked runs it between
	// the branch_facts and the branches deletes (SetOnDropBranchRows).
	onDropBranchRows func(ctx context.Context, branchID int64) error

	// writeOnt caches the ontology validateWrite last parsed.
	writeOnt OntCache
	// agentID is this instance's persisted agent id (SetAgentID), named by
	// every authored commit. Never derived from a branch name.
	agentID string

	// replaySkip (SetReplaySkipPaths) is the set of paths a rewind replay
	// never carries: a non-merge commit that changes nothing but these is
	// dropped from the replay (unpushedCommits).
	replaySkip map[string]bool

	// changes holds each derived commit's tree diff until its commit_log rows
	// are built from it: one diff per commit (changeLists).
	changes changeLists

	onCommit func(branch, hash string) // external observer (e.g. SSE broadcast)

	// consensusBranch is remotesync.Index.UpstreamBranch (reached as
	// Service.Remote().UpstreamBranch): the branch whose tip carries
	// the repo's `conflicts` setting, the one every other merge site reads it
	// at. Set by Open; nil only on a Repo built bare in a test.
	consensusBranch func() string

	// acceptList is the operator's accept list (control.db), bound to this
	// repository: the entry waiving a commit, if any. Nil is empty. Set by
	// Service.SetAcceptList at build/swap time, never mutated afterwards.
	acceptList func(commit plumbing.Hash) (model.Accept, bool)

	// ownKeys, when set, returns every key this instance's fleet member record
	// has held (E4 accepts them as its own). Nil when standalone.
	ownKeys func() []ssh.PublicKey

	// syncIndex is index.Index.Sync (installed by Service.Open through
	// SetIndexSync; vcs cannot import the index package, which sits above
	// it). NotifyCommit calls it after every commit so branch_facts /
	// facts_vec / graph stay in sync with the new tree at HEAD. It is
	// lock-FREE: NotifyCommit runs under LockBranch(branch). If nil,
	// NotifyCommit skips the sync — useful for bare Repo tests that
	// don't exercise the index.
	syncIndex func(ctx context.Context, branch string) error

	// readOnly refuses authored commits at the shared write seam — the
	// structural half of a subscription (see Service.SetReadOnly). Set by
	// Service.SetReadOnly at build/swap time before the store is published,
	// never mutated afterwards, so no lock is needed. It does NOT survive
	// store.Open, so it must be re-applied on SwapStore reopen exactly like
	// SetOntologyRoot — a swap that forgets it silently makes a subscription
	// writable again.
	readOnly bool

	name string // repo name, derived from dbPath at Open time

	// factRoot is the ontology root — the one directory facts live under, and
	// therefore the only part of the tree the index admits (see IsFactPath).
	// Empty means "not configured": OntologyRoot() falls back to the default,
	// so a bare Repo built without a Service still behaves. Set by
	// Service.SetOntologyRoot from cfg.OntologyRoot at build/swap time, never
	// mutated afterwards, so no lock is needed.
	factRoot string

	// netTimeout bounds every remote git network operation (clone/fetch/push/
	// ls-remote). 0 means no bound (hang-forever, the legacy behavior). Set by
	// Service.SetNetworkTimeout from cfg.Git.NetworkTimeout at build/swap time;
	// read at each go-git call site via NetCtx. Never mutated after wiring, so
	// no lock is needed.
	netTimeout time.Duration

	configMu sync.RWMutex // write: configureRemote refspec rewrite; read: held across a fetch so a rewrite can't race it
	embedMu  sync.RWMutex // guards embedder
	embedder model.Embedder
	branchMu sync.Map // per-branch write serialization
	// pushMu serializes PUSHES of a branch (remotesync.Index.Push), and only
	// pushes: it is not the write lock, so a fact write never waits on it.
	pushMu sync.Map

	// gitTreeReads counts calls to ReadFileAtCommit + ReadBlobHashAtCommit —
	// the two commit-tree lookups that dominate rebuildGraph's I/O. It is
	// cheap observability (one atomic add per go-git tree walk) and lets
	// rebuild-path tests assert that each (commit, path) is read from git at
	// most once rather than once per phase. Never load-bearing for behaviour.
	gitTreeReads atomic.Int64
}

// GitTreeReadCount returns the number of commit-tree reads performed so far.
// Test-only observability into rebuild/graph I/O; see Repo.gitTreeReads.
func (rh *Repo) GitTreeReadCount() int64 { return rh.gitTreeReads.Load() }

// LockBranch acquires the per-branch write lock and returns an unlock function.
// Used by facts.Index, remotesync.Index, and MergeBranch to serialize writes on a
// given branch AND to exclude concurrent Verify runs (which take the read side
// via LockBranchRead). The lock is held for the full mutation — from the
// initial ref resolution through SetReference AND NotifyCommit's index sync —
// so no reader can observe the window between "git HEAD advanced" and "SQL
// branch_facts / graph updated".
func (rh *Repo) LockBranch(branch string) func() {
	v, _ := rh.branchMu.LoadOrStore(branch, &sync.RWMutex{})
	mu := v.(*sync.RWMutex)
	mu.Lock()
	return mu.Unlock
}

// LockPush acquires the per-branch push mutex (see remotesync.Index.Push) and
// returns its unlock. It is held for a whole push, network included, which is
// why it must never be the write lock.
func (rh *Repo) LockPush(branch string) func() {
	v, _ := rh.pushMu.LoadOrStore(branch, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// LockBranchRead acquires the per-branch read lock. Multiple readers can hold
// it concurrently, but no writer can proceed while any reader holds it. Used
// by Verify to take a consistent snapshot of a branch's git tree + SQL index
// state without blocking parallel read-only operations.
func (rh *Repo) LockBranchRead(branch string) func() {
	v, _ := rh.branchMu.LoadOrStore(branch, &sync.RWMutex{})
	mu := v.(*sync.RWMutex)
	mu.RLock()
	return mu.RUnlock
}

// NetCtx derives a deadline-bounded context for a single remote git network
// operation from parent. When netTimeout is 0 the parent is returned unchanged
// (no bound — legacy hang-forever behavior) with a no-op cancel. Callers MUST
// always defer the returned cancel. Pass context.Background() at call sites that
// have no inbound context (CloneFrom/InitFromRemote/detect*).
func (rh *Repo) NetCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return NetCtxWith(parent, rh.netTimeout)
}

// NetCtxWith is the free-function form of NetCtx for call sites (e.g. the
// package-level fetchOrigin helper) that hold a timeout value but no
// Repo. timeout<=0 returns parent unchanged with a no-op cancel.
func NetCtxWith(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// SetEmbedder attaches an Embedder. Called once during service construction.
func (rh *Repo) SetEmbedder(e model.Embedder) {
	rh.embedMu.Lock()
	defer rh.embedMu.Unlock()
	rh.embedder = e
}

// Embedder returns the current Embedder under a read lock.
func (rh *Repo) Embedder() model.Embedder {
	rh.embedMu.RLock()
	defer rh.embedMu.RUnlock()
	return rh.embedder
}

func New(db *sql.DB, gits *storegit.Storer) *Repo {
	return &Repo{db: db, cache: newBranchCache(), gits: gits}
}

// EnsureBranch creates the branch if it doesn't exist, returns its ID.
func (rh *Repo) EnsureBranch(ctx context.Context, name, gitRef string) (int64, error) {
	if id, ok := rh.cache.get(name); ok {
		return id, nil
	}

	_, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx,
		`INSERT OR IGNORE INTO branches(name, git_ref) VALUES (?, ?)`,
		name, gitRef,
	)
	if err != nil {
		return 0, fmt.Errorf("ensure branch: %w", err)
	}

	var id int64
	err = sqlite.Conn(ctx, rh.db).QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure branch lookup: %w", err)
	}

	rh.cache.set(name, id)
	return id, nil
}

// branchID returns the ID for a branch name, using the cache when possible.
// Returns an error if the branch does not exist.
func (rh *Repo) BranchID(ctx context.Context, name string) (int64, error) {
	if id, ok := rh.cache.get(name); ok {
		return id, nil
	}

	var id int64
	err := sqlite.Conn(ctx, rh.db).QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("branch %q: %w", name, model.ErrBranchNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("branch lookup: %w", err)
	}

	rh.cache.set(name, id)
	return id, nil
}

// MergeBranch lives in branch_merge.go alongside mergeTreesWithStrategy.

// DropBranch removes a branch from both the git storer (ref deletion) and
// every SQLite table that references it. Follows the blueprint's promise
// that deleting a branch "does not affect other branches": the git ref is
// gone, `branches` is gone, `branch_facts` is gone, `branch_commits` is
// gone via ON DELETE CASCADE, and the orphaned commits themselves remain
// in the object store (shared via COW — other branches may still reach
// them). Verify's branches-table check catches any residue.
func (rh *Repo) DropBranch(ctx context.Context, name string) error {
	id, err := rh.BranchID(ctx, name)
	if err != nil {
		return fmt.Errorf("drop branch: %w", err)
	}

	// Acquire the per-branch write lock so concurrent writes can't race
	// the deletion. Released when the function returns.
	unlock := rh.LockBranch(name)
	defer unlock()
	return rh.DropBranchLocked(ctx, name, id)
}

// DropBranchLocked is DropBranch's body, for a caller that already holds
// name's write lock (demotePeer decides under it whether to drop).
func (rh *Repo) DropBranchLocked(ctx context.Context, name string, id int64) error {
	// 0. Fail closed, before anything is removed, on a Repo whose builder never
	// registered the step-2b deletes: running without them would leave the
	// rows behind and fail step 3 after the ref is gone.
	if rh.onDropBranchRows == nil {
		return ErrNoDropBranchRowsHook
	}
	// 1. Delete the git ref first. If this fails, we leave the SQLite
	// state untouched so the operation is retryable — better than a
	// half-removed state where the ref is gone but rows remain.
	if err := rh.gits.RemoveReference(plumbing.NewBranchReferenceName(name)); err != nil {
		return fmt.Errorf("drop branch: remove git ref: %w", err)
	}

	// 2. Delete branch_facts rows (branch_commits cascades from branches).
	if _, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx, `DELETE FROM branch_facts WHERE branch_id = ?`, id); err != nil {
		return fmt.Errorf("drop branch_facts: %w", err)
	}
	// 2b. Delete the rows higher clusters keep per branch in tables that
	// reference branches(id) WITHOUT a cascade (the review pipeline's derived
	// state). Foreign keys are enforced (_foreign_keys=1), so leaving them
	// would fail step 3 — after the git ref is already gone, which is the
	// half-removed state step 1 exists to avoid. This package does not know
	// which tables those are: the parent registers the deletes with
	// SetOnDropBranchRows when it builds the Repo, and they run here, with this
	// ctx (so inside the caller's transaction when there is one), under the
	// branch's write lock, and their error is returned unchanged.
	if err := rh.onDropBranchRows(ctx, id); err != nil {
		return err
	}
	// 2c. Delete the branch's index watermark and in-flight sync marker
	// (meta last_commit:<name>, sync_pending:<name>). Left behind, the
	// watermark would mark a dropped branch as maintained (fsck's
	// indexedBranches) and, since CreateBranch inherits the parent's
	// watermark with INSERT OR IGNORE, a branch recreated under the name
	// would keep the dropped one's watermark instead; a stale marker would
	// keep the recreated branch unsettled (review R2-1). The keys are spelled
	// as the index spells them.
	if _, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx,
		`DELETE FROM meta WHERE key IN (?, ?)`, "last_commit:"+name, "sync_pending:"+name); err != nil {
		return fmt.Errorf("drop branch meta: %w", err)
	}
	// 3. Delete the branches row. This cascades into branch_commits via
	// the FK defined in migration 000004.
	if _, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx, `DELETE FROM branches WHERE id = ?`, id); err != nil {
		return fmt.Errorf("drop branch row: %w", err)
	}

	rh.cache.remove(name)

	if rh.onDrop != nil {
		return rh.onDrop(ctx)
	}
	return nil
}

// ListBranches returns all registered branches.
func (rh *Repo) ListBranches(ctx context.Context) ([]model.Branch, error) {
	rows, err := sqlite.Conn(ctx, rh.db).QueryContext(ctx, `SELECT id, name, git_ref FROM branches ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	var branches []model.Branch
	for rows.Next() {
		var b model.Branch
		if err := rows.Scan(&b.ID, &b.Name, &b.GitRef); err != nil {
			return nil, fmt.Errorf("list branches scan: %w", err)
		}
		branches = append(branches, b)
	}
	return branches, rows.Err()
}

// CreateBranch creates a new git branch ref pointing at the tip of fromBranch.
// The child branch inherits the parent's full state: git ref position,
// branch_commits visibility, AND branch_facts (the per-branch view of which
// fact version is visible at each path). This matches git's "branch contains
// everything the parent contains at the moment of creation" semantics.
//
// No-op if the branch already exists.
func (rh *Repo) CreateBranch(ctx context.Context, branch, fromBranch string) error {
	newRefName := plumbing.NewBranchReferenceName(branch)
	if _, err := rh.gits.Reference(newRefName); err == nil {
		return nil // already exists
	}
	fromHash, err := rh.ResolveRef(ctx, fromBranch)
	if err != nil {
		return fmt.Errorf("CreateBranch: resolve source %q: %w", fromBranch, err)
	}
	// The copied visibility below must only name derived commits: the
	// source's commits are (indexing derives them), so this is a lookup —
	// unless the source predates path_changes, when it derives them now.
	if err := rh.DeriveBeforeAdvance(ctx, fromHash); err != nil {
		return fmt.Errorf("CreateBranch: %w", err)
	}
	if err := rh.gits.SetReference(plumbing.NewHashReference(newRefName, fromHash)); err != nil {
		return fmt.Errorf("CreateBranch: set ref: %w", err)
	}
	newBranchID, err := rh.EnsureBranch(ctx, branch, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("CreateBranch: ensure branches row for %q: %w", branch, err)
	}
	// Clone parent branch's visibility: every commit visible on fromBranch is
	// now also visible on the new branch. This matches git's "branch contains
	// all parent commits" semantics without copying commit_log rows.
	if _, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx, `
		INSERT OR IGNORE INTO branch_commits (branch_id, commit_hash)
		SELECT ?, commit_hash FROM branch_commits
		WHERE branch_id = (SELECT id FROM branches WHERE name = ?)`,
		newBranchID, fromBranch); err != nil {
		return fmt.Errorf("CreateBranch: clone branch_commits from %q: %w", fromBranch, err)
	}
	// Clone parent branch's per-path fact view: every branch_facts row on
	// fromBranch becomes a row on the new branch pointing at the same
	// (fact_id, commit_hash). Without this step, the child's tree at HEAD
	// contains inherited files that have no matching branch_facts row, which
	// Verify's facts-coherence check correctly flags as a gap.
	if _, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx, `
		INSERT OR IGNORE INTO branch_facts (branch_id, path, fact_id, commit_hash)
		SELECT ?, path, fact_id, commit_hash FROM branch_facts
		WHERE branch_id = (SELECT id FROM branches WHERE name = ?)`,
		newBranchID, fromBranch); err != nil {
		return fmt.Errorf("CreateBranch: clone branch_facts from %q: %w", fromBranch, err)
	}
	// Inherit the parent's sync watermark so the child's first im.Sync does
	// an efficient incremental diff from the same baseline rather than a
	// wasteful full rebuild that would re-upsert every already-indexed fact.
	var parentWatermark string
	err = sqlite.Conn(ctx, rh.db).QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, "last_commit:"+fromBranch,
	).Scan(&parentWatermark)
	if err == nil && parentWatermark != "" {
		if _, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx,
			`INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`,
			"last_commit:"+branch, parentWatermark,
		); err != nil {
			return fmt.Errorf("CreateBranch: inherit watermark: %w", err)
		}
	}
	log.Info().Str("branch", branch).Str("from", fromBranch).Msg("created branch")
	return nil
}

// DefaultBranch resolves the default branch name from the repo's HEAD ref.
// Returns an empty string if HEAD is detached.
func (rh *Repo) DefaultBranch(_ context.Context) (string, error) {
	head, err := rh.gits.Reference(plumbing.HEAD)
	if err != nil {
		return "", fmt.Errorf("DefaultBranch: resolve HEAD: %w", err)
	}
	if head.Type() == plumbing.SymbolicReference {
		return strings.TrimPrefix(head.Target().String(), "refs/heads/"), nil
	}
	return "", nil
}

// SetDefaultBranch sets the symbolic HEAD to point at the given branch.
func (rh *Repo) SetDefaultBranch(branch string) error {
	return rh.gits.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)),
	)
}

// agentBranchOwnerKey is the meta key recording which agent branch this repo
// database is currently written by. See AgentBranchOwner.
const agentBranchOwnerKey = "agent_branch_owner"

// AgentBranchOwner returns the agent branch this database records as its
// current owner, or "" if none has been recorded yet.
//
// The agent branch is the instance's persisted identity (control.db
// instance_identity, built once at first boot by app.newIdentity). Recording
// the owner makes "which branch does this database write to" an explicit stored fact
// rather than something re-derived, so an instance whose derived name no
// longer matches can seed a new branch from the recorded one instead of
// guessing from HEAD or from branch-name shape.
//
// An empty result means either a database created before this key existed or
// one that has never completed a boot; callers must treat it as "unknown", not
// as "no previous branch".
func (rh *Repo) AgentBranchOwner(ctx context.Context) (string, error) {
	var owner string
	err := rh.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, agentBranchOwnerKey).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("AgentBranchOwner: %w", err)
	}
	return owner, nil
}

// SetAgentBranchOwner records branch as this database's agent branch owner,
// replacing any previous value. Call only once the branch is known to exist:
// a recorded owner that has no branch would make the next boot seed from a
// missing ref.
func (rh *Repo) SetAgentBranchOwner(ctx context.Context, branch string) error {
	if _, err := rh.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		agentBranchOwnerKey, branch); err != nil {
		return fmt.Errorf("SetAgentBranchOwner: %w", err)
	}
	return nil
}

// ConfigureRemote ensures origin is registered with a fetch refspec for the
// upstream consensus branch (upstreamMain, whatever the origin names) and, when
// this machine has one, a second for its agent branch. Idempotent. Both branch
// names are part of their respective refspecs, so callers must pass the same
// upstreamMain and agentBranch on every call for a given repo. upstreamMain is
// never empty here: Service.ConfigureRemote refuses it, and the init paths
// resolve it first.
//
// An EMPTY agentBranch means a subscription: the repo follows the upstream
// read-only and has no branch of its own, so exactly one refspec is written.
// Interpolating an empty name would otherwise produce "+refs/heads/:refs/
// remotes/origin/", which git accepts into the config and which then makes
// every fetch fail.
func (rh *Repo) ConfigureRemote(url, upstreamMain, agentBranch string) error {
	rh.configMu.Lock()
	defer rh.configMu.Unlock()

	cfg, err := rh.repo.Config()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	mainRefspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", upstreamMain, upstreamMain)
	// A subscription has no agent branch and tracks the upstream alone.
	want := []gogitconfig.RefSpec{gogitconfig.RefSpec(mainRefspec)}
	if agentBranch != "" {
		want = append(want, gogitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", agentBranch, agentBranch)))
	}

	if rc, ok := cfg.Remotes["origin"]; ok && len(rc.URLs) > 0 && rc.URLs[0] == url {
		if sameRefspecs(rc.Fetch, want) {
			return nil // already configured
		}
	}

	_ = rh.repo.DeleteRemote("origin")
	_, err = rh.repo.CreateRemote(&gogitconfig.RemoteConfig{Name: "origin", URLs: []string{url}, Fetch: want})
	return err
}

// sameRefspecs reports set equality of two refspec lists.
func sameRefspecs(a, b []gogitconfig.RefSpec) bool {
	if len(a) != len(b) {
		return false
	}
	got := make(map[gogitconfig.RefSpec]bool, len(a))
	for _, rs := range a {
		got[rs] = true
	}
	for _, rs := range b {
		if !got[rs] {
			return false
		}
	}
	return true
}

// ── git read methods ──────────────────────────────────────────────────────────

// ResolveRef returns the commit hash at the tip of branch.
func (rh *Repo) ResolveRef(ctx context.Context, branch string) (plumbing.Hash, error) {
	ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return plumbing.ZeroHash, fmt.Errorf("resolveRef %q: %w: %w", branch, model.ErrBranchNotFound, err)
		}
		return plumbing.ZeroHash, fmt.Errorf("resolveRef %q: %w", branch, err)
	}
	return ref.Hash(), nil
}

// ReadFileAtCommit reads the content of path from a specific commit.
// If the exact path is not found, it falls back to a case-insensitive tree
// walk so that normalised (lowercase) index paths resolve correctly against
// pre-normalisation commits that stored paths with mixed case.
func (rh *Repo) ReadFileAtCommit(ctx context.Context, path, commitHash string) (string, error) {
	noteGitPathRead("ReadFileAtCommit")
	rh.gitTreeReads.Add(1)
	hash := plumbing.NewHash(commitHash)
	commit, err := rh.repo.CommitObject(hash)
	if err != nil {
		return "", fmt.Errorf("readFileAtCommit: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("readFileAtCommit: tree: %w", err)
	}
	if f, err := tree.File(path); err == nil {
		return f.Contents()
	}
	// Exact lookup failed — try case-insensitive walk.
	content, err := treeFileInsensitive(rh.repo, tree, path)
	if err != nil {
		if errors.Is(err, model.ErrPathNotFound) {
			return "", fmt.Errorf("readFileAtCommit: %q at %s: %w", path, commitHash, model.ErrPathNotFound)
		}
		return "", fmt.Errorf("readFileAtCommit: file %q (case-insensitive): %w", path, err)
	}
	return content, nil
}

// FirstParentCommit returns the first parent of commitHash, or "" if
// commitHash is a root commit. For merge commits, this returns the "ours"
// side (parent[0]) — the branch that was checked out when the merge was
// performed. Walking first-parent traces a single branch's local history,
// which is what the time-aware DERIVED_FROM resolver needs to find the
// "active version of path P at commit C on branch B" without being
// confused by versions merged in from sibling branches.
func (rh *Repo) FirstParentCommit(ctx context.Context, commitHash string) (string, error) {
	commit, err := rh.repo.CommitObject(plumbing.NewHash(commitHash))
	if err != nil {
		return "", fmt.Errorf("firstParentCommit: %s: %w", commitHash, err)
	}
	if commit.NumParents() == 0 {
		return "", nil
	}
	return commit.ParentHashes[0].String(), nil
}

// ReadBlobHashAtCommit returns the blob hash for path in the git tree at
// commitHash. Used to bridge (path, commit) → blob_hash when wiring graph
// edges that point to a specific Fact(path, blob_hash) version.
//
// No case-insensitive fallback: refs are stored normalised and target_commit
// is known-good (returned by resolveTargetCommit), so an exact lookup is
// sufficient. Compare with ReadFileAtCommit above which falls back for
// pre-normalisation legacy paths.
func (rh *Repo) ReadBlobHashAtCommit(ctx context.Context, path, commitHash string) (string, error) {
	noteGitPathRead("ReadBlobHashAtCommit")
	rh.gitTreeReads.Add(1)
	hash := plumbing.NewHash(commitHash)
	commit, err := rh.repo.CommitObject(hash)
	if err != nil {
		return "", fmt.Errorf("readBlobHashAtCommit: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("readBlobHashAtCommit: tree: %w", err)
	}
	f, err := tree.File(path)
	if err != nil {
		return "", fmt.Errorf("readBlobHashAtCommit: file %q at %s: %w", path, commitHash, err)
	}
	return f.Hash.String(), nil
}

// HeadCommit returns the hash of the tip commit of branch as a hex string.
//
// Honours ctx cancellation before touching the ref store so a cancelled or
// expired context surfaces as ctx.Err() — NOT as a missing ref. Callers that
// distinguish "branch absent" (ErrBranchNotFound) from "lookup failed" (e.g.
// repos.validateLensLocked, which maps the former to 4xx and the latter to
// 500) rely on this: ResolveRef reads the ref via go-git without consulting
// ctx, so without this guard a cancelled lookup would be indistinguishable
// from a present branch.
func (rh *Repo) HeadCommit(ctx context.Context, branch string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	hash, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	return hash.String(), nil
}

// HeadCommitInfo returns both the HEAD commit hash and its committer
// timestamp. Used by the cluster-cache background checker to detect "activity
// has settled for N seconds" before triggering a recompute.
func (rh *Repo) HeadCommitInfo(ctx context.Context, branch string) (string, time.Time, error) {
	hash, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("HeadCommitInfo: %w", err)
	}
	commit, err := rh.repo.CommitObject(hash)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("HeadCommitInfo: commit object: %w", err)
	}
	return hash.String(), commit.Committer.When, nil
}

// BlobAtCommit returns the blob hash of path in commit's tree, or "" when the
// path is absent there.
func (rh *Repo) BlobAtCommit(commitHash plumbing.Hash, path string) (string, error) {
	commit, err := rh.repo.CommitObject(commitHash)
	if err != nil {
		return "", fmt.Errorf("blobAtCommit: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("blobAtCommit: tree: %w", err)
	}
	entry, err := tree.FindEntry(path)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("blobAtCommit: entry %s: %w", path, err)
	}
	return entry.Hash.String(), nil
}

// ReadFileWithHash returns both the file content and the blob hash for the given path.
func (rh *Repo) ReadFileWithHash(ctx context.Context, branch, path string) (string, string, error) {
	headHash, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: ref: %w", err)
	}
	commit, err := rh.repo.CommitObject(headHash)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: tree: %w", err)
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return "", "", fmt.Errorf("readFileWithHash: %q: %w", path, model.ErrPathNotFound)
		}
		return "", "", fmt.Errorf("readFileWithHash: entry %s: %w", path, err)
	}
	blob, err := rh.repo.BlobObject(entry.Hash)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: blob: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: reader: %w", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: read: %w", err)
	}
	return string(b), entry.Hash.String(), nil
}

// readFile reads the content of path from the tip of branch.
func (rh *Repo) ReadFile(ctx context.Context, branch, path string) (string, error) {
	content, _, err := rh.ReadFileWithHash(ctx, branch, path)
	return content, err
}

// pathHashSorter sorts two parallel slices (paths and hashes) together by path.
type pathHashSorter struct{ paths, hashes []string }

func (ps pathHashSorter) Len() int           { return len(ps.paths) }
func (ps pathHashSorter) Less(i, j int) bool { return ps.paths[i] < ps.paths[j] }
func (ps pathHashSorter) Swap(i, j int) {
	ps.paths[i], ps.paths[j] = ps.paths[j], ps.paths[i]
	ps.hashes[i], ps.hashes[j] = ps.hashes[j], ps.hashes[i]
}

// ListAllWithHash returns all .md files at the tip of branch with their blob hashes.
// Single tree walk — no per-file I/O.
func (rh *Repo) ListAllWithHash(ctx context.Context, branch string) ([]string, []string, error) {
	headHash, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: ref: %w", err)
	}

	commit, err := rh.repo.CommitObject(headHash)
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: commit: %w", err)
	}

	fileIter, err := commit.Files()
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: files: %w", err)
	}
	defer fileIter.Close()

	var paths, blobHashes []string
	err = fileIter.ForEach(func(f *object.File) error {
		if strings.HasSuffix(f.Name, ".md") {
			paths = append(paths, f.Name)
			blobHashes = append(blobHashes, f.Hash.String())
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: iterate: %w", err)
	}

	sort.Sort(pathHashSorter{paths, blobHashes})
	return paths, blobHashes, nil
}

// ListAll returns paths of all .md files at the tip of branch.
func (rh *Repo) ListAll(ctx context.Context, branch string) ([]string, error) {
	paths, _, err := rh.ListAllWithHash(ctx, branch)
	return paths, err
}

// LastCommitForPath returns the hash of the most recent non-merge commit
// that touched path. Merges are skipped because they duplicate authoring
// commits from the merged branch.
func (rh *Repo) LastCommitForPath(ctx context.Context, branch, path string) (string, error) {
	headHash, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("LastCommitForPath: ref: %w", err)
	}

	logIter, err := rh.repo.Log(&gogit.LogOptions{
		From:     headHash,
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return "", fmt.Errorf("LastCommitForPath: log: %w", err)
	}
	defer logIter.Close()

	for {
		c, err := logIter.Next()
		if err != nil {
			return "", fmt.Errorf("LastCommitForPath: %q: no commit found", path)
		}
		// Skip merge commits (more than one parent).
		if c.NumParents() <= 1 {
			return c.Hash.String(), nil
		}
	}
}

// DiffFiles returns paths added/modified/deleted between fromCommit and the tip of branch.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (rh *Repo) DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error) {
	headHash, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: ref: %w", err)
	}

	toCommit, err := rh.repo.CommitObject(headHash)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to commit: %w", err)
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to tree: %w", err)
	}

	var fromTree *object.Tree
	if fromCommit != "" {
		fromHash := plumbing.NewHash(fromCommit)
		fc, err := rh.repo.CommitObject(fromHash)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from commit: %w", err)
		}
		fromTree, err = fc.Tree()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from tree: %w", err)
		}
	}

	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: diff tree: %w", err)
	}

	for _, ch := range changes {
		from := ch.From.Name
		to := ch.To.Name

		switch {
		case from == "" && to != "":
			if strings.HasSuffix(to, ".md") {
				added = append(added, to)
			}
		case from != "" && to == "":
			if strings.HasSuffix(from, ".md") {
				deleted = append(deleted, from)
			}
		default:
			if strings.HasSuffix(to, ".md") {
				modified = append(modified, to)
			}
		}
	}

	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(deleted)
	return added, modified, deleted, nil
}

// BranchInfo returns all branches partitioned into regular branches, agent
// branches (prefixed "agent/"), and the agent branch matching localAgent (if any).
func (rh *Repo) BranchInfo(localAgent string) (branches, agentBranches []string, matchedAgent string) {
	refIter, err := rh.gits.IterReferences()
	if err != nil {
		return
	}
	defer refIter.Close()

	agentSet := make(map[string]struct{})
	for {
		ref, err := refIter.Next()
		if err != nil {
			break
		}
		name := ref.Name().String()
		if !strings.HasPrefix(name, "refs/heads/") {
			continue
		}
		short := strings.TrimPrefix(name, "refs/heads/")
		if strings.HasPrefix(short, "agent/") {
			if _, seen := agentSet[short]; !seen {
				agentSet[short] = struct{}{}
				if short == localAgent {
					matchedAgent = short
				}
			}
		} else {
			branches = append(branches, short)
		}
	}
	agentBranches = make([]string, 0, len(agentSet))
	for b := range agentSet {
		agentBranches = append(agentBranches, b)
	}
	return
}

// ── Commit-log wrappers ───────────────────────────────────────────────────────
// These methods hide storegit cursor types and branch-ID resolution from callers.

// CommitLogQuery resolves branch to its numeric ID and delegates to
// storegit.CommitLogQuery, hiding cursor construction from callers.
func (rh *Repo) CommitLogQuery(ctx context.Context, branch, path, after, from, before string, limit int) ([]storegit.CommitLogRow, bool, error) {
	branchID, _ := rh.BranchID(ctx, branch) // 0 on error → no filter
	var cursor storegit.CommitLogCursor
	switch {
	case before != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorBefore, Hash: before}
	case from != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorFrom, Hash: from}
	case after != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorAfter, Hash: after}
	}
	return rh.gits.CommitLogQuery(branchID, path, cursor, limit)
}

// CommitLogActivity resolves branch to its numeric ID and delegates to
// storegit.CommitLogActivity.
func (rh *Repo) CommitLogActivity(ctx context.Context, branch, path string, cutoff7, cutoff30, cutoff90 int64) (storegit.CommitLogActivityResult, error) {
	branchID, _ := rh.BranchID(ctx, branch)
	return rh.gits.CommitLogActivity(branchID, path, cutoff7, cutoff30, cutoff90)
}

// CommitLogFileCounts delegates to storegit.CommitLogFileCounts.
// No branch filter needed — commit hashes already identify the branch.
func (rh *Repo) CommitLogFileCounts(hashes []string) (map[string]map[string]int, error) {
	return rh.gits.CommitLogFileCounts(hashes)
}
