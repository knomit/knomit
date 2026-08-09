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

// HomeDir returns the Storyboard's root tempdir — the same root every
// BareRemote/BareRemoteHTTP fixture is served from (<HomeDir>/remotes/<name>)
// and every Repo's per-repo manager home is nested under
// (<HomeDir>/<name>). It exists for tests that must build their OWN
// repos.Manager sharing this Storyboard's remote fixtures and local-origin
// allowlist directly — Repo cannot serve that need because it deliberately
// boots a SEPARATE manager (and therefore a separate control.db) per repo
// name, isolating repos from one another. A test asserting a constraint that
// spans repos in ONE registry (e.g. the identity-uniqueness guard on Create)
// has to opt out of that isolation and drive its own Manager instead, while
// still reusing the Storyboard's bare-remote fixtures and LocalOriginRoot.
func (sb *Storyboard) HomeDir() string { return sb.homeDir }

// Repo returns (or creates) a RepoHandle named `name`. Each repo gets its own
// manager rooted in a per-repo subdirectory of the Storyboard's tempdir, holding
// exactly one repo, itself named `name`.
//
// Booting a manager creates nothing — knomit has no default repo — so the repo
// is created explicitly here via the production Manager.Create path.
func (sb *Storyboard) Repo(name string) *RepoHandle {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if r, ok := sb.repos[name]; ok {
		return r
	}

	cfg := sb.repoConfig(name)
	m, err := sb.bootManager(cfg, "")
	if err != nil {
		sb.t.Fatalf("Repo(%q): manager boot failed: %v", name, err)
	}
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: name, Mode: "preset"}, nil)
	if err != nil {
		sb.t.Fatalf("Repo(%q): create failed: %v", name, err)
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

// credentialKey writes (idempotently) the Storyboard's agent key file and
// returns its path. The manager reads it (Deps.KeyPath) to build the Crypt that
// Origins.Set demands before it will persist an origin credential — credentials
// are never stored in plaintext — so a manager that clones WITH credentials
// must be booted with this path or the clone dies at persist-origin.
//
// It is deliberately not the default for every manager: the auth-resolution
// contract cell depends on Deps.KeyPath being empty so an ssh remote has no key
// to fall back on.
func (sb *Storyboard) credentialKey() (string, error) {
	path := filepath.Join(sb.homeDir, "agent.key")
	if err := os.WriteFile(path, []byte("storyboard-agent-key"), 0o600); err != nil {
		return "", fmt.Errorf("write agent key: %w", err)
	}
	return path, nil
}

// repoConfig builds the config for one RepoHandle's private manager home.
func (sb *Storyboard) repoConfig(name string) config.Config {
	cfg := config.Config{Home: filepath.Join(sb.homeDir, name)}
	// Bound every storyboard-driven remote git op with a SHORT timeout. Real
	// test clones run over local file:// / loopback HTTP and complete in well
	// under a second, so 5s never trips them — but a deliberately-hung remote
	// (the clone-stall contract cell) aborts at 5s, far inside that test's 20s
	// budget, instead of hanging forever. Storyboard cfg is not built via
	// config.Defaults(), so without this the timeout would be 0 (no bound).
	cfg.Git.NetworkTimeout = 5 * time.Second
	// Allow file:// remotes created under the Storyboard sandbox: the local-origin
	// gate (validateLocalOrigin) blocks local-path origins unless LocalOriginRoot
	// is set. BareRemote builds remotes at <homeDir>/remotes/<name>, so the
	// sandbox root authorizes exactly those fixtures and nothing outside.
	cfg.LocalOriginRoot = sb.homeDir
	if sb.methodologyMinScore != nil {
		cfg.MethodologyMinScore = *sb.methodologyMinScore
	}
	return cfg
}

// bootManager starts a manager over cfg's home. It opens whatever repos already
// exist there and creates none, so a fresh home comes up empty.
//
// keyPath is the agent key the manager reads; "" leaves the manager without
// credential encryption, which is what most repos want (see credentialKey).
func (sb *Storyboard) bootManager(cfg config.Config, keyPath string) (*repos.Manager, error) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   cfg,
		AgentBranch:           "agent/test",
		Embedder:              sb.embedder,
		KeyPath:               keyPath,
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		return nil, err
	}
	return m, nil
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
	// originURL is the remote this repo is currently wired to, "" when it has
	// none. The Connect family reads it for their idempotency check — the origin
	// lives in control.db's repo_origins, keyed by the repo's uid, never in cfg.
	originURL string
	// keyPath is the agent key this repo's manager boots with, "" when it needs
	// no credential encryption. Connect sets it for a credentialled clone; every
	// later re-boot must reuse it or the persisted origin token stops decrypting.
	keyPath     string
	branches    map[string]*BranchHandle
	expectDirty bool
}

// WithRemoteAuth sets the credentials this repo uses against origin. It MUST be
// called before Connect / ConnectKeepingWork so the initial clone/reconcile
// authenticates. Returns the RepoHandle for chaining.
//
// Connect maps these onto the create spec's OriginSpec (see originAuth), which
// is where the production clone path reads them from: initClone resolves the
// SPEC's credential, not the server-level cfg.Remote, and Create persists it in
// control.db for the reconcile loop to reuse. cfg.Remote still feeds
// makeRemoteAuthFn's fallback and SyncAuthed.
func (r *RepoHandle) WithRemoteAuth(auth config.RemoteAuthConfig) *RepoHandle {
	r.cfg.Remote = auth
	return r
}

// originAuth maps this repo's configured credentials onto the (AuthMethod,
// AuthToken) pair an OriginSpec carries, mirroring authConfigFromSpec's
// convention: basic packs "user:password" into the token. Returns ("", "") when
// no credential is configured, which clones anonymously.
func (r *RepoHandle) originAuth() (method, token string) {
	a := r.cfg.Remote
	switch {
	case a.AuthMethod == "basic" || (a.AuthMethod == "" && (a.User != "" || a.Password != "")):
		return "basic", a.User + ":" + a.Password
	case a.AuthMethod == "token" || (a.AuthMethod == "" && a.Token != ""):
		return "token", a.Token
	default:
		return "", ""
	}
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

// SetOriginAuth rewrites the credential on this repo's PERSISTED origin record,
// leaving the URL and upstream branch untouched, and re-injects the amended
// origin into the live store so the next reconcile resolves auth from it
// without a reopen.
//
// It is the control.db successor to `UPDATE remotes SET auth_method = ...`:
// connection identity (url, branch, auth) now lives in <home>/control.db, and
// GetRemote prefers the injected origin over the repo's legacy remotes row — so
// a contract cell that needs a repo configured with a deliberately-broken
// credential has to write it where the reconcile loop actually reads it.
//
// Storing a non-empty token still requires the manager to have a Crypt (see
// credentialKey); Origins.Set refuses a plaintext credential.
func (r *RepoHandle) SetOriginAuth(method, token string) {
	t := r.sb.t
	t.Helper()
	origins := r.manager.Origins()
	if origins == nil {
		t.Fatalf("SetOriginAuth(%q): manager has no origin store", r.name)
	}
	cur, err := origins.Get(r.ri.UID())
	if err != nil {
		t.Fatalf("SetOriginAuth(%q): read persisted origin: %v", r.name, err)
	}
	if cur == nil {
		t.Fatalf("SetOriginAuth(%q): repo has no origin to reconfigure", r.name)
	}
	cur.AuthMethod = method
	cur.AuthToken = token
	if err := origins.Set(r.ri.UID(), *cur); err != nil {
		t.Fatalf("SetOriginAuth(%q): persist origin: %v", r.name, err)
	}
	r.ri.WithRead(func(svc *store.Service) {
		svc.SetOrigin(&store.Origin{
			URL:        cur.URL,
			Branch:     cur.Branch,
			AuthMethod: cur.AuthMethod,
			AuthToken:  cur.AuthToken,
		})
	})
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

// Connect wires this repo to use the given RemoteHandle as its origin. It
// shuts down the current manager, discards the local databases, re-boots into
// an empty home, and re-creates the repo in clone mode. That drives the
// production Manager.Create → InitFromRemote path — exactly what knomit does
// when a user creates a repo from an origin — which either clones the remote's
// existing state (non-empty remote) or seeds the repo inline (empty remote) and
// registers "origin" in both the git config and the remotes SQLite table.
//
// The clone is created with NO explicit branch so InitFromRemote resolves the
// upstream against the remote itself (prefer "main", else its symbolic HEAD);
// that is the behaviour a master-convention remote depends on.
//
// This approach mirrors production semantics precisely: no hand-rolled fetch,
// no direct ref manipulation, no bypassing of index synchronization. The
// trade-off is that Connect MUST be called before any Branch() writes — the
// databases are deleted here, so a test that writes first and connects later
// would lose its data.
//
// Calling Connect() twice is a no-op after the first successful call.
func (r *RepoHandle) Connect(remote *RemoteHandle) *RepoHandle {
	t := r.sb.t
	t.Helper()

	if err := r.connect(remote); err != nil {
		t.Fatal(err)
	}
	return r
}

// TryConnect is the error-returning, goroutine-safe variant of Connect. It
// drives the SAME production clone path but returns any error instead of
// calling t.Fatalf, so a contract cell can run it under a deadline (e.g. to
// detect that a clone against a stalled remote never aborts). It touches no
// *testing.T and so is safe to invoke from a goroutine. Like Connect, it must
// be called before any Branch() writes; calling it after a successful Connect
// to the same origin is a no-op.
func (r *RepoHandle) TryConnect(remote *RemoteHandle) error {
	return r.connect(remote)
}

// connect is the shared implementation of Connect/TryConnect. It touches no
// *testing.T so both the fatal and the error-returning wrapper can use it.
func (r *RepoHandle) connect(remote *RemoteHandle) error {
	if r.originURL == remote.URL() {
		return nil // idempotent
	}

	// The Storyboard creates repos via preset mode, which leaves the local
	// SQLite DB populated with an unrelated init commit. To drive the clone path
	// cleanly, dispose of that repo entirely so the re-booted manager comes up
	// with an empty home and the create below is a true first clone.
	//
	// Deleting the .db files is NOT enough any more: the registry lives in
	// <home>/control.db, so a repo whose file vanished leaves an ACTIVE row
	// behind — the re-booted manager reports it "missing" and the Create below
	// fails with `repo already exists` because the row still claims the name.
	// Archive + Purge is the production disposal path: Archive flips the row to
	// archived (freeing both the name and the knowledge-base identity, whose
	// unique indexes are partial-on-active) and releases the SQLite handle;
	// Purge deletes the archived registry row (cascading to its stored
	// credential) and then the database file — there are no manifests any more,
	// the registry in control.db replaced them. Both must run
	// BEFORE Close, which drops the manager's control.db handles.
	//
	// Either failing leaves the handle without a usable store, so mark it dirty
	// for the same reason the clone failures below do: teardown's auto-verify
	// must not run against a half-disposed repo.
	info, err := r.manager.Archive(r.name)
	if err != nil {
		r.expectDirty = true
		return fmt.Errorf("connect(%s): archive existing repo: %w", remote.Name(), err)
	}
	if err := r.manager.Purge(info.ID); err != nil {
		r.expectDirty = true
		return fmt.Errorf("connect(%s): purge archived repo: %w", remote.Name(), err)
	}
	r.manager.Close()

	// From here on a failure leaves this handle with no live store (the prior
	// manager is closed, and the replacement did not come up). Mark the repo
	// dirty so teardown's auto-verify skips it rather than failing on a closed
	// DB — the caller is asserting on the returned error, not on the handle's
	// post-failure integrity. Safe to set without a lock: the caller observes it
	// only after receiving the returned error (a happens-before edge), and
	// teardown runs strictly after that.
	//
	// A credentialled clone also needs a key path: Create persists the origin
	// credential, and Origins.Set refuses to store one without a Crypt.
	authMethod, authToken := r.originAuth()
	if authMethod != "" {
		keyPath, kerr := r.sb.credentialKey()
		if kerr != nil {
			r.expectDirty = true
			return fmt.Errorf("connect(%s): %w", remote.Name(), kerr)
		}
		r.keyPath = keyPath
	}
	m, err := r.sb.bootManager(r.cfg, r.keyPath)
	if err != nil {
		r.expectDirty = true
		return fmt.Errorf("connect(%s): re-boot failed: %w", remote.Name(), err)
	}
	ri, err := m.Create(context.Background(), repos.CreateSpec{
		Name: r.name,
		Mode: "clone",
		Origin: &repos.OriginSpec{
			URL:        remote.URL(),
			AuthMethod: authMethod,
			AuthToken:  authToken,
		},
	}, nil)
	if err != nil {
		r.expectDirty = true
		return fmt.Errorf("connect(%s): clone failed: %w", remote.Name(), err)
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
// (PUT /api/v1/{repo}/origin → persist origin + wire git remote + ActivateSync)
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

	// Persist the origin in control.db, inject it into the live store so
	// GetRemote and the sync paths see it without a reopen, then write the git
	// config so go-git can fetch/push by name. This is the three-step the HAL
	// origin handler runs; control.db is the source of truth and the git config
	// is a derived cache.
	if err := r.manager.Origins().Set(r.ri.UID(), repos.Origin{URL: remote.URL(), Branch: remote.UpstreamBranch()}); err != nil {
		t.Fatalf("ConnectKeepingWork(%s): persist origin: %v", remote.Name(), err)
	}
	var setErr error
	r.ri.WithRead(func(svc *store.Service) {
		svc.SetOrigin(&store.Origin{URL: remote.URL(), Branch: remote.UpstreamBranch()})
		setErr = svc.ConfigureRemote(remote.URL(), remote.UpstreamBranch(), "agent/test")
	})
	if setErr != nil {
		t.Fatalf("ConnectKeepingWork(%s): configure remote: %v", remote.Name(), setErr)
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
// remote via the SAME production path (persist the origin in control.db, inject
// it, rewrite the git remote, then ri.ActivateSync's synchronous reconcile) but
// returns any error instead of calling t.Fatalf, so a cell can run it under a
// deadline (e.g. to
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

	if err := r.manager.Origins().Set(r.ri.UID(), repos.Origin{URL: remote.URL(), Branch: remote.UpstreamBranch()}); err != nil {
		return fmt.Errorf("TryReConnect(%s): persist origin: %w", remote.Name(), err)
	}
	var setErr error
	r.ri.WithRead(func(svc *store.Service) {
		svc.SetOrigin(&store.Origin{URL: remote.URL(), Branch: remote.UpstreamBranch()})
		setErr = svc.ConfigureRemote(remote.URL(), remote.UpstreamBranch(), "agent/test")
	})
	if setErr != nil {
		return fmt.Errorf("TryReConnect(%s): configure remote: %w", remote.Name(), setErr)
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

	m, err := r.sb.bootManager(r.cfg, r.keyPath)
	if err != nil {
		t.Fatalf("Restart(%q): manager re-boot failed: %v", r.name, err)
	}
	ri := m.Get(r.name)
	if ri == nil {
		t.Fatalf("Restart(%q): repo not re-opened from disk after boot", r.name)
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
