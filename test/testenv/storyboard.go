package testenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

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

// StoryRepoName is the name of the one repo every Storyboard scenario works
// in. knomit has no default repo — nothing is created implicitly at boot — so
// the harness creates this one explicitly through Manager.Create and every
// handle refers to it by this name. Scenario-level isolation comes from each
// RepoHandle having its OWN manager and home directory, not from the name.
const StoryRepoName = "core"

// storyKeyPath is the fake agent key each scenario home carries. It exists
// only so the store gets a Crypt: a clone-mode Create persists the origin's
// auth token, and SetRemote REFUSES to write a credential without encryption.
// The same path is handed to every manager built against a given home, so the
// derived key — and therefore every stored token — survives a re-boot.
func storyKeyPath(home string) string { return filepath.Join(home, "agent_key") }

// ensureStoryKey writes the fake agent key if it is not already there.
func ensureStoryKey(home string) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	path := storyKeyPath(home)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte("storyboard-agent-key"), 0o600)
}

// originSpec builds the clone-mode origin spec for url, carrying whatever
// credentials WithRemoteAuth configured. The product used to read those off the
// server-wide cfg.Remote during the default repo's first-run clone; with the
// default repo gone, an origin's credentials travel with the CreateSpec that
// introduces it, so the harness maps them the same way an operator's POST
// /api/v1/repos body would.
func (r *RepoHandle) originSpec(url string) *repos.OriginSpec {
	spec := &repos.OriginSpec{URL: url}
	a := r.cfg.Remote
	switch {
	case a.AuthMethod == "basic" || (a.AuthMethod == "" && (a.User != "" || a.Password != "")):
		spec.AuthMethod = "basic"
		spec.AuthToken = a.User + ":" + a.Password
	case a.AuthMethod == "token" || (a.AuthMethod == "" && a.Token != ""):
		spec.AuthMethod = "token"
		spec.AuthToken = a.Token
	}
	return spec
}

// createStoryRepo creates StoryRepoName in m through the production
// Manager.Create path and returns the live instance.
func createStoryRepo(m *repos.Manager, spec repos.CreateSpec) (*repos.RepoInstance, error) {
	spec.Name = StoryRepoName
	ri, err := m.Create(context.Background(), spec, nil)
	if err != nil {
		return nil, err
	}
	if ri == nil {
		return nil, fmt.Errorf("create %q: manager returned no instance", StoryRepoName)
	}
	return ri, nil
}

// Repo returns (or creates) a RepoHandle named `name`. Each repo gets its
// own manager rooted in a per-repo subdirectory of the Storyboard's tempdir.
// The manager boots empty on first access — knomit creates no repo on its own —
// and the harness then creates StoryRepoName inside that home dir.
func (sb *Storyboard) Repo(name string) *RepoHandle {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if r, ok := sb.repos[name]; ok {
		return r
	}

	homeSub := filepath.Join(sb.homeDir, name)
	cfg := config.Config{Home: homeSub}
	// Bound every storyboard-driven remote git op with a SHORT timeout. Real
	// test clones run over local file:// / loopback HTTP and complete in well
	// under a second, so 5s never trips them — but a deliberately-hung remote
	// (the clone-stall contract cell) aborts at 5s, far inside that test's 20s
	// budget, instead of hanging forever. Storyboard cfg is not built via
	// config.Defaults(), so without this the timeout would be 0 (no bound).
	cfg.Git.NetworkTimeout = 5 * time.Second
	// Allow file:// remotes created under the Storyboard sandbox: the local-origin
	// gate (validateLocalOrigin) blocks local-path origins unless
	// LocalOriginRoot is set. BareRemote builds remotes at <homeDir>/remotes/<name>,
	// so the sandbox root authorizes exactly those fixtures and nothing outside.
	cfg.LocalOriginRoot = sb.homeDir
	if sb.methodologyMinScore != nil {
		cfg.MethodologyMinScore = *sb.methodologyMinScore
	}
	if err := ensureStoryKey(homeSub); err != nil {
		sb.t.Fatalf("Repo(%q): write agent key: %v", name, err)
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   cfg,
		AgentBranch:           "agent/test",
		Embedder:              sb.embedder,
		KeyPath:               storyKeyPath(homeSub),
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		sb.t.Fatalf("Repo(%q): manager boot failed: %v", name, err)
	}
	// Mode "preset" with no preset id seeds the default ontology — the same
	// seed files the removed default-repo bootstrap used to write.
	ri, err := createStoryRepo(m, repos.CreateSpec{Mode: "preset"})
	if err != nil {
		sb.t.Fatalf("Repo(%q): create %q failed: %v", name, StoryRepoName, err)
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
	sb      *Storyboard
	name    string
	ri      *repos.RepoInstance
	manager *repos.Manager
	cfg     config.Config
	// originURL is the origin this handle has been wired to, or "" when it has
	// none. It replaces the old server-wide cfg.Git.Origin: an origin belongs to
	// a repo now, so the handle tracks its own for Connect's idempotency check.
	originURL   string
	branches    map[string]*BranchHandle
	expectDirty bool
}

// WithRemoteAuth sets the RemoteAuthConfig that the product uses to
// authenticate against origin (cfg.Remote). It MUST be called before Connect /
// ConnectKeepingWork so the initial clone/reconcile authenticates. This mirrors
// how an operator supplies credentials via config: the same cfg.Remote is
// threaded into resolveAuth (initial clone) and makeRemoteAuthFn (reconcile
// loop). Returns the RepoHandle for chaining.
func (r *RepoHandle) WithRemoteAuth(auth config.RemoteAuthConfig) *RepoHandle {
	r.cfg.Remote = auth
	return r
}

// remoteAuth builds the go-git AuthMethod the product would resolve from this
// repo's cfg.Remote for a basic/token remote. Used by SyncAuthed to drive the
// production Sync path with the SAME credentials the reconcile loop would use,
// so a contract cell can observe how a now-invalid token is handled. Returns
// nil (anonymous) when no auth is configured.
func (r *RepoHandle) remoteAuth() transport.AuthMethod {
	a := r.cfg.Remote
	switch {
	case a.AuthMethod == "basic" || (a.AuthMethod == "" && (a.User != "" || a.Password != "")):
		return &githttp.BasicAuth{Username: a.User, Password: a.Password}
	case a.AuthMethod == "token" || (a.AuthMethod == "" && a.Token != ""):
		user := a.User
		if user == "" {
			user = "x-token"
		}
		return &githttp.BasicAuth{Username: user, Password: a.Token}
	default:
		return nil
	}
}

// SyncAuthed runs one production Sync reconcile using the credentials
// configured via WithRemoteAuth and returns the resulting SyncResult and any
// error WITHOUT calling t.Fatalf — the error-returning, auth-carrying variant
// of BranchHandle.Sync. It drives svc.Remote().Sync (which persists last_status
// / last_error on every return), so after calling it a contract cell can read
// RemoteStatus() to assert the failure was recorded rather than silently
// swallowed.
func (r *RepoHandle) SyncAuthed(agentBranch string) (store.SyncResult, error) {
	var result store.SyncResult
	var err error
	auth := r.remoteAuth()
	r.ri.WithRead(func(svc *store.Service) {
		result, err = svc.Remote().Sync(context.Background(), agentBranch, auth)
	})
	return result, err
}

// RemoteStatus reads the persisted remote record for "origin" via the store,
// exposing LastStatus / LastError so contract cells can assert the visible
// sync status after a reconcile. Returns nil if no origin is configured.
func (r *RepoHandle) RemoteStatus() *store.Remote {
	var rec *store.Remote
	r.ri.WithRead(func(svc *store.Service) {
		rec, _ = svc.Remote().GetRemote("origin")
	})
	return rec
}

// OriginAuthMethod reads the auth METHOD control.db holds for this repo's
// origin — the control-plane half of what RemoteStatus reads out of the store.
// Credentials moved out of the repo's own database into control.db, and
// Manager.OriginAuth reads them from there with no fallback, so this is the only
// place a contract cell can observe what auth a reconcile is about to attempt.
//
// It deliberately returns the method ALONE. The stored token is a secret that no
// assertion needs, and a helper that handed it back would invite it into a
// failure message.
func (r *RepoHandle) OriginAuthMethod() string {
	reg := r.manager.RepoRegistry()
	if reg == nil {
		r.sb.t.Fatalf("OriginAuthMethod on repo %q: no registry", r.name)
	}
	method, _, err := reg.OriginCredential(StoryRepoName)
	if err != nil {
		r.sb.t.Fatalf("OriginAuthMethod on repo %q: %v", r.name, err)
	}
	return method
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
// It shuts down the current manager, discards the on-disk databases, re-boots
// (to an empty manager — knomit creates no repo on its own), and re-creates the
// repo in clone mode. That drives the production Manager.Create → initClone →
// InitFromRemote path — exactly what knomit does when a user creates a repo
// from an origin — which either clones the remote's existing state (non-empty
// remote) or seeds the repo inline (empty remote) and registers "origin" in
// both the git config and the remotes SQLite table.
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

	if r.originURL == remote.URL() {
		return r // idempotent
	}

	// The Storyboard creates repos in preset mode, which leaves the local
	// SQLite DB populated with an unrelated init commit on main. To
	// drive the production clone path cleanly we shut the manager down,
	// blow away the on-disk DB files, and re-create the repo in clone
	// mode. Manager.Create → initClone → InitFromRemote then clones from
	// the remote (or seeds from scratch against an empty remote) exactly
	// as production would.
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

	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   r.cfg,
		AgentBranch:           "agent/test",
		Embedder:              r.sb.embedder,
		KeyPath:               storyKeyPath(r.cfg.Home),
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("Connect(%s): re-boot failed: %v", remote.Name(), err)
	}
	// Branch is left empty on purpose: the clone path must detect the remote's
	// symbolic HEAD, so a master-default remote stays master everywhere.
	ri, err := createStoryRepo(m, repos.CreateSpec{
		Mode:   "clone",
		Origin: r.originSpec(remote.URL()),
	})
	if err != nil {
		t.Fatalf("Connect(%s): clone-create failed: %v", remote.Name(), err)
	}
	r.originURL = remote.URL()
	r.manager = m
	r.ri = ri
	r.branches = map[string]*BranchHandle{}
	r.sb.mu.Lock()
	r.sb.managers[r.name] = m
	r.sb.mu.Unlock()
	return r
}

// TryConnect is the error-returning, goroutine-safe variant of Connect. It
// drives the SAME production clone path (manager re-boot, then a clone-mode
// Create) but returns any error instead of calling t.Fatalf, so a
// contract cell can run it under a deadline (e.g. to detect that a clone
// against a stalled remote never aborts). It touches no *testing.T and so is
// safe to invoke from a goroutine. Like Connect, it must be called before any
// Branch() writes; calling it after a successful Connect to the same origin is
// a no-op.
func (r *RepoHandle) TryConnect(remote *RemoteHandle) error {
	if r.originURL == remote.URL() {
		return nil // idempotent
	}

	r.manager.Close()

	reposDir := filepath.Join(r.cfg.Home, "repos")
	entries, _ := os.ReadDir(reposDir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(reposDir, e.Name())
		if err := os.Remove(full); err != nil {
			return fmt.Errorf("TryConnect(%s): remove %s: %w", remote.Name(), full, err)
		}
	}

	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   r.cfg,
		AgentBranch:           "agent/test",
		Embedder:              r.sb.embedder,
		KeyPath:               storyKeyPath(r.cfg.Home),
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		// The re-boot left no live store on this handle (the prior manager was
		// already closed above, and the new one failed to come up). Mark the
		// repo dirty so teardown's auto-verify skips it rather than failing on a
		// closed DB — the caller is asserting on the returned error, not on the
		// handle's post-failure integrity. Safe to set without a lock: the
		// caller observes this only after receiving the returned error (a
		// happens-before edge), and teardown runs strictly after that.
		r.expectDirty = true
		return fmt.Errorf("TryConnect(%s): re-boot failed: %w", remote.Name(), err)
	}
	ri, err := createStoryRepo(m, repos.CreateSpec{
		Mode:   "clone",
		Origin: r.originSpec(remote.URL()),
	})
	if err != nil {
		r.expectDirty = true
		return fmt.Errorf("TryConnect(%s): clone-create failed: %w", remote.Name(), err)
	}
	r.originURL = remote.URL()
	r.manager = m
	r.ri = ri
	r.branches = map[string]*BranchHandle{}
	r.sb.mu.Lock()
	r.sb.managers[r.name] = m
	r.sb.mu.Unlock()
	return nil
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

	if r.originURL == remote.URL() {
		return r // already connected
	}
	r.originURL = remote.URL()

	// Register origin in the remotes table AND write the git config; the
	// rh.repo guard inside SetRemote means configureRemote runs because
	// the repo handler already has an open repo from InitRepo.
	var setErr error
	r.ri.WithRead(func(svc *store.Service) {
		setErr = svc.Remote().SetRemote("origin", remote.URL(), remote.UpstreamBranch(), "agent/test", 300, 300, "", "")
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

// TryReConnect is the error-returning, goroutine-safe variant of
// ConnectKeepingWork. It RE-POINTS an already-connected repo's origin to a new
// remote via the SAME production path (svc.Remote().SetRemote +
// ri.ActivateSync's synchronous reconcile) but returns any error instead of
// calling t.Fatalf, so a contract cell can run it under a deadline (e.g. to
// detect that re-pointing to a hung remote never aborts). It touches no
// *testing.T and is safe to invoke from a goroutine. Unlike TryConnect it does
// NOT wipe the DB — it mirrors the PUT /api/v1/{repo}/origin re-point flow.
//
// Idempotent: re-pointing to the already-configured URL returns nil.
func (r *RepoHandle) TryReConnect(remote *RemoteHandle) error {
	if r.originURL == remote.URL() {
		return nil
	}
	r.originURL = remote.URL()

	var setErr error
	r.ri.WithRead(func(svc *store.Service) {
		setErr = svc.Remote().SetRemote("origin", remote.URL(), remote.UpstreamBranch(), "agent/test", 300, 300, "", "")
	})
	if setErr != nil {
		return fmt.Errorf("TryReConnect(%s): SetRemote: %w", remote.Name(), setErr)
	}

	// ActivateSync runs one synchronous reconcile (fetch bounded by the
	// configured network timeout) exactly like the HAL handler. If the new
	// remote hangs, this must abort at the timeout rather than block forever.
	if err := r.ri.ActivateSync(remote.URL()); err != nil {
		return fmt.Errorf("TryReConnect(%s): ActivateSync: %w", remote.Name(), err)
	}
	return nil
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
		KeyPath:               storyKeyPath(r.cfg.Home),
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("Restart(%q): manager re-boot failed: %v", r.name, err)
	}
	ri := m.Get(StoryRepoName)
	if ri == nil {
		t.Fatalf("Restart(%q): repo %q is missing after re-boot", r.name, StoryRepoName)
	}
	r.manager = m
	r.ri = ri
	r.branches = map[string]*BranchHandle{}
	r.sb.mu.Lock()
	r.sb.managers[r.name] = m
	r.sb.mu.Unlock()
}

// RestartWithEmbedder restarts the repo using a different embedder, simulating
// a config change to a new embedding model. The next index open (setupIndex →
// NeedsRebuild) detects the embedding-identity change and re-embeds the corpus.
// Like Restart, all existing BranchHandle references become stale afterward.
func (r *RepoHandle) RestartWithEmbedder(e store.BatchEmbedder) {
	r.sb.embedder = e
	r.Restart()
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
