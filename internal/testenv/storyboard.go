package testenv

import (
	"context"
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
}

// Storyboard is the root of a test scenario. It owns a tempdir, a
// per-repo repos.Manager, and registers t.Cleanup to auto-verify every
// tracked repo before tearing down.
type Storyboard struct {
	t        *testing.T
	homeDir  string
	embedder store.BatchEmbedder
	auto     bool
	deep     bool
	mu       sync.Mutex
	repos    map[string]*RepoHandle
	managers map[string]*repos.Manager
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
		t:        t,
		homeDir:  t.TempDir(),
		embedder: embedder,
		auto:     opts.AutoVerify,
		deep:     opts.VerifyDeep,
		repos:    make(map[string]*RepoHandle),
		managers: make(map[string]*repos.Manager),
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
		m.Shutdown()
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
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         cfg,
		AgentBranch: "agent/test",
		Embedder:    sb.embedder,
	})
	if err := m.Boot(); err != nil {
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
// Calls store.Remote().SetRemote with file:// URL, main as the default
// remote branch, and no auth. Returns the same RepoHandle for chaining:
//
//	agent := sb.Repo("a").Connect(remote).Branch("agent/test")
//
// Connect is idempotent — calling it twice with the same remote is harmless
// (SetRemote is INSERT OR REPLACE under the hood).
func (r *RepoHandle) Connect(remote *RemoteHandle) *RepoHandle {
	t := r.sb.t
	t.Helper()
	var setErr error
	r.ri.WithRead(func(svc *store.Service) {
		setErr = svc.Remote().SetRemote(
			"origin",
			remote.URL(),
			"main", // remote branch
			300,    // sync interval seconds (unused in tests)
			300,    // push interval seconds (unused in tests)
			"",     // auth method (file:// needs none)
			"",     // auth token
		)
	})
	if setErr != nil {
		t.Fatalf("Connect(%s): SetRemote: %v", remote.Name(), setErr)
	}
	return r
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
