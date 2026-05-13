package testenv

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// StoryboardOpts controls Storyboard behavior. Zero value uses sensible
// defaults: AutoVerify and VerifyDeep both true, DeterministicEmbedder
// as the embedder.
type StoryboardOpts struct {
	Embedder   store.BatchEmbedder // nil → DeterministicEmbedder
	AutoVerify bool                // default true
	VerifyDeep bool                // default true
	// MethodologyMinScore overrides the per-repo methodology threshold
	// when non-nil. Storyboard cfg is not loaded via config.Defaults(),
	// so the default (0) admits every candidate; tests that need to
	// observe threshold filtering set this explicitly.
	MethodologyMinScore *float64
}

// Storyboard is the root of a test scenario. It owns a tempdir, a
// per-repo repos.Manager, and registers t.Cleanup to auto-verify every
// tracked repo before tearing down.
type Storyboard struct {
	t                   *testing.T
	homeDir             string
	embedder            store.BatchEmbedder
	auto                bool
	deep                bool
	methodologyMinScore *float64
	mu                  sync.Mutex
	repos               map[string]*RepoHandle
	managers            map[string]*repos.Manager
}

// NewStoryboard creates a Storyboard with default options. Most tests use this.
func NewStoryboard(t *testing.T) *Storyboard {
	return NewStoryboardWithOpts(t, StoryboardOpts{AutoVerify: true, VerifyDeep: true})
}

// NewStoryboardWithOpts creates a Storyboard with explicit options.
func NewStoryboardWithOpts(t *testing.T, opts StoryboardOpts) *Storyboard {
	t.Helper()
	embedder := opts.Embedder
	if embedder == nil {
		embedder = &DeterministicEmbedder{}
	}
	sb := &Storyboard{
		t:                   t,
		homeDir:             t.TempDir(),
		embedder:            embedder,
		auto:                opts.AutoVerify,
		deep:                opts.VerifyDeep,
		methodologyMinScore: opts.MethodologyMinScore,
		repos:               make(map[string]*RepoHandle),
		managers:            make(map[string]*repos.Manager),
	}
	t.Cleanup(sb.teardown)
	return sb
}

// teardown runs on test completion. Auto-verifies every tracked repo that
// was not marked ExpectDirty, then shuts down every manager.
func (sb *Storyboard) teardown() {
	sb.mu.Lock()
	repoList := make([]*RepoHandle, 0, len(sb.repos))
	for _, r := range sb.repos {
		repoList = append(repoList, r)
	}
	managerList := make([]*repos.Manager, 0, len(sb.managers))
	for _, m := range sb.managers {
		managerList = append(managerList, m)
	}
	sb.mu.Unlock()

	for _, r := range repoList {
		if !r.expectDirty {
			AssertIntegrity(sb.t, r)
		}
	}
	for _, m := range managerList {
		m.Close()
	}
}

// Repo returns (or creates) a RepoHandle named `name`. Each repo gets its
// own manager rooted in a per-repo subdirectory of the Storyboard's tempdir.
// The manager boots on first access, which creates the default "knomit"
// repo database inside that home dir.
func (sb *Storyboard) Repo(name string) *RepoHandle {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if r, ok := sb.repos[name]; ok {
		return r
	}

	homeSub := filepath.Join(sb.homeDir, name)
	cfg := config.Config{Home: homeSub}
	if sb.methodologyMinScore != nil {
		cfg.MethodologyMinScore = *sb.methodologyMinScore
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   cfg,
		AgentBranch:           "agent/test",
		Embedder:              sb.embedder,
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		sb.t.Fatalf("Repo(%q): manager boot failed: %v", name, err)
	}
	ri := m.Get("knomit")
	if ri == nil {
		sb.t.Fatalf("Repo(%q): manager.Get(knomit) returned nil after Boot", name)
	}

	r := &RepoHandle{
		sb:       sb,
		name:     name,
		ri:       ri,
		manager:  m,
		cfg:      cfg,
		branches: map[string]*BranchHandle{},
	}
	sb.repos[name] = r
	sb.managers[name] = m
	return r
}

// RepoHandle wraps a repos.RepoInstance with DSL ergonomics. Created by
// Storyboard.Repo. Holds branch handles, the parent Storyboard, and an
// "expect dirty" flag for G-category corruption tests.
type RepoHandle struct {
	sb          *Storyboard
	name        string
	ri          *repos.RepoInstance
	manager     *repos.Manager
	cfg         config.Config
	branches    map[string]*BranchHandle
	expectDirty bool
}

// Branch returns (or creates) a BranchHandle for the named branch. The
// branch must already exist in the repo — Task 2.4 will add BranchFrom for
// creating new branches.
func (r *RepoHandle) Branch(name string) *BranchHandle {
	if b, ok := r.branches[name]; ok {
		return b
	}
	b := &BranchHandle{repo: r, name: name}
	r.branches[name] = b
	return b
}

// BranchFrom creates a new branch named `name` as a child of `fromBranch`
// (the child inherits every commit reachable from the parent at creation
// time). Returns a BranchHandle for the newly-created branch. Fails the
// test if the create fails — use for scenario tests that need a fresh
// child branch to diverge from an existing one.
func (r *RepoHandle) BranchFrom(name, fromBranch string) *BranchHandle {
	t := r.sb.t
	t.Helper()
	if _, ok := r.branches[name]; ok {
		t.Fatalf("BranchFrom: branch %q already tracked by DSL", name)
	}
	var createErr error
	r.ri.WithRead(func(svc *store.Service) {
		createErr = svc.Branches().CreateBranch(context.Background(), name, fromBranch)
	})
	if createErr != nil {
		t.Fatalf("BranchFrom(%s from %s): %v", name, fromBranch, createErr)
	}
	b := &BranchHandle{repo: r, name: name}
	r.branches[name] = b
	return b
}

// Connect wires this repo to use the given RemoteHandle as its origin.
// It shuts down the current manager, sets cfg.Git.Origin to the
// remote's file:// URL, and re-boots the manager. The re-boot drives
// the production InitFromRemote path — exactly what knomit does in
// production when a repo is opened with an origin configured — which
// either clones the remote's existing state (non-empty remote) or
// seeds the repo inline (empty remote) and registers "origin" in both
// the git config and the remotes SQLite table.
//
// This approach mirrors production semantics precisely: no hand-
// rolled fetch, no direct ref manipulation, no bypassing of index
// synchronization. The trade-off is that Connect MUST be called
// before any Branch() writes — the re-boot discards all of the
// initial manager's in-memory state including branch handles. The
// on-disk SQLite database is the same file, so anything persisted
// survives.
//
// Calling Connect() twice is a no-op after the first successful
// call.
func (r *RepoHandle) Connect(remote *RemoteHandle) *RepoHandle {
	t := r.sb.t
	t.Helper()

	if r.cfg.Git.Origin == remote.URL() {
		return r // idempotent
	}

	// The Storyboard boots repos via InitRepo, which leaves the local
	// SQLite DB populated with an unrelated init commit on main. To
	// drive the production InitFromRemote path cleanly we shut the
	// manager down, blow away the on-disk DB files, and re-boot with
	// cfg.Git.Origin set. repoBuilder.openGit → initDefaultGit →
	// InitFromRemote then clones from the remote (or seeds from
	// scratch against an empty remote) exactly as production would.
	//
	// This is why Connect MUST be called BEFORE any Branch() writes —
	// wiping the DB is destructive. Tests that write first and
	// connect later aren't supported and would lose their data here.
	r.manager.Close()

	reposDir := filepath.Join(r.cfg.Home, "repos")
	entries, _ := os.ReadDir(reposDir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(reposDir, e.Name())
		if err := os.Remove(full); err != nil {
			t.Fatalf("Connect(%s): remove %s: %v", remote.Name(), full, err)
		}
	}

	r.cfg.Git.Origin = remote.URL()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   r.cfg,
		AgentBranch:           "agent/test",
		Embedder:              r.sb.embedder,
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("Connect(%s): re-boot failed: %v", remote.Name(), err)
	}
	ri := m.Get("knomit")
	if ri == nil {
		t.Fatalf("Connect(%s): manager.Get(knomit) returned nil after re-boot", remote.Name())
	}
	r.manager = m
	r.ri = ri
	r.branches = map[string]*BranchHandle{}
	r.sb.mu.Lock()
	r.sb.managers[r.name] = m
	r.sb.mu.Unlock()
	return r
}

// ConnectKeepingWork wires this repo to use the given RemoteHandle as
// origin WITHOUT wiping the local DB. The use case is the G2 "connect
// later" scenario: tests accumulate local work on the agent branch
// before any origin is configured, then call ConnectKeepingWork to
// model a user running `knomit set-origin` after they've already used
// the agent for offline edits. Mirrors the production HAL flow
// (PUT /api/v1/{repo}/origin → svc.Remote().SetRemote + ri.ActivateSync)
// exactly — no destructive re-init, the existing branch refs and
// SQLite rows survive, and ActivateSync runs one synchronous reconcile
// that should fetch origin and replay the local commits onto the
// resolved upstream.
//
// Idempotent: calling with the already-configured remote URL is a no-op.
// Returns the same RepoHandle for chaining.
func (r *RepoHandle) ConnectKeepingWork(remote *RemoteHandle) *RepoHandle {
	t := r.sb.t
	t.Helper()

	if r.cfg.Git.Origin == remote.URL() {
		return r // already connected
	}
	r.cfg.Git.Origin = remote.URL()

	// Register origin in the remotes table AND write the git config; the
	// rh.repo guard inside SetRemote means configureRemote runs because
	// the repo handler already has an open repo from InitRepo.
	var setErr error
	r.ri.WithRead(func(svc *store.Service) {
		setErr = svc.Remote().SetRemote("origin", remote.URL(), "agent/test", 300, 300, "", "")
	})
	if setErr != nil {
		t.Fatalf("ConnectKeepingWork(%s): SetRemote: %v", remote.Name(), setErr)
	}

	// Trigger one synchronous reconcile via the production ActivateSync
	// path. This is exactly what the HAL handler does on
	// PUT /api/v1/{repo}/origin.
	if err := r.ri.ActivateSync(remote.URL()); err != nil {
		t.Fatalf("ConnectKeepingWork(%s): ActivateSync: %v", remote.Name(), err)
	}
	return r
}

// Restart shuts down the current manager and re-boots a fresh one against
// the same on-disk home directory. Used by Category I "survives restart"
// tests to assert that the index persists across process boundaries. All
// existing BranchHandle references become stale after Restart — callers
// must re-fetch via Branch(name).
//
// The parent Storyboard's managers map is updated to point at the new
// manager so teardown calls Shutdown on the live instance.
func (r *RepoHandle) Restart() {
	t := r.sb.t
	t.Helper()
	r.manager.Close()

	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   r.cfg,
		AgentBranch:           "agent/test",
		Embedder:              r.sb.embedder,
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("Restart(%q): manager re-boot failed: %v", r.name, err)
	}
	ri := m.Get("knomit")
	if ri == nil {
		t.Fatalf("Restart(%q): manager.Get(knomit) returned nil after Boot", r.name)
	}
	r.manager = m
	r.ri = ri
	r.branches = map[string]*BranchHandle{}
	r.sb.mu.Lock()
	r.sb.managers[r.name] = m
	r.sb.mu.Unlock()
}

// ExpectDirty marks the repo as deliberately corrupted. The Storyboard
// teardown auto-verify will skip this repo. Call after CorruptObject /
// RawSQL / RawGitWrite in G-category tests.
func (r *RepoHandle) ExpectDirty() { r.expectDirty = true }

// Instance is the escape hatch for tests that need direct RepoInstance
// access. Prefer the DSL methods where possible.
func (r *RepoHandle) Instance() *repos.RepoInstance { return r.ri }

// Name returns the repo's Storyboard-assigned name.
func (r *RepoHandle) Name() string { return r.name }

// MustVerify runs Verify(Deep: true) and fails the test if not clean.
func (r *RepoHandle) MustVerify() { AssertIntegrity(r.sb.t, r) }

// VerifyWith returns the report without asserting; caller inspects it.
// Used by G-category tests that want to inspect specific issues.
func (r *RepoHandle) VerifyWith(opts store.VerifyOpts) store.IntegrityReport {
	report, err := r.ri.Verify(context.Background(), opts)
	if err != nil {
		r.sb.t.Fatalf("VerifyWith on repo %q: %v", r.name, err)
	}
	return report
}

// BranchHandle is a per-branch DSL handle. Mutations (Write, Update,
// Delete) auto-commit and return a Snapshot pinning the resulting commit.
// Every mutation also runs AssertIntegrity on the repo unless the parent
// Storyboard has AutoVerify disabled.
type BranchHandle struct {
	repo      *RepoHandle
	name      string
	snapshots []*Snapshot
}

// Name returns the branch's git ref name (without the refs/heads/ prefix).
func (b *BranchHandle) Name() string { return b.name }

// SnapshotsForTest exposes the snapshot stack for unit tests within the
// testenv package. Do not use from scenario tests — use At/AtIndex/AtName
// (Task 2.7) or the return values of mutation methods instead.
func (b *BranchHandle) SnapshotsForTest() []*Snapshot { return b.snapshots }
