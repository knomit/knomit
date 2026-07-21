package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/store"
)

// Deps holds all shared resources needed to open and manage repos.
type Deps struct {
	Cfg         config.Config
	Signer      ssh.Signer
	AgentBranch string
	Embedder    store.BatchEmbedder // nil if unavailable
	KeyPath     string
	// DisableBackgroundSync suppresses the background pull and push loops
	// that would otherwise run on every managed repo. Tests use this to
	// prevent non-deterministic sync/push behavior — the loops call
	// doSync/doPush immediately on startup which can race with test
	// assertions about remote state. Production leaves this unset.
	DisableBackgroundSync bool
}

// RescanError records a single per-repo failure during Manager.Rescan.
// Top-level errors (e.g. unreadable repos directory) are returned via the
// second return value of Rescan, not in this slice.
type RescanError struct {
	Repo string
	Err  error
}

// RescanResult is the outcome of a Manager.Rescan call.
//
//   - Added: repo names that were not previously registered and were
//     successfully opened during this call.
//   - Skipped: repo names already registered before this call.
//   - Errors: per-repo Add failures; other repos still attempted.
//
// On successful return, all three slices are non-nil; empty slices remain
// empty (callers/JSON encoders can rely on []string{} rather than nil).
// When Rescan returns a non-nil error, the caller should not inspect the
// result — the zero RescanResult{} is returned.
type RescanResult struct {
	Added   []string
	Skipped []string
	Errors  []RescanError
}

// Manager owns the full lifecycle of all registered repositories:
// discovery, initialisation, MCP wiring, sync loop management, the
// background cluster-cache warmer, and shutdown. Callers drive the
// lifecycle via Start/Close — internals (sync loops, cluster checker)
// are not exposed.
type Manager struct {
	mu    sync.RWMutex
	repos map[string]*RepoInstance
	ctx   context.Context
	deps  Deps

	// sessionReaperStop is set by Start when the background idle-session
	// reaper is launched, and invoked by Close to wind it down. nil only when
	// Start hasn't been called — the reaper itself is never disabled (see
	// parseSessionReaperConfig).
	sessionReaperStop func()

	// registry is the lens registry (first tenant of <home>/control.db).
	// Opened by Start, closed by Close; nil before Start.
	registry *LensRegistry

	// settings is the per-repo settings store (second tenant of
	// <home>/control.db). Opened by Start, closed by Close; nil before Start.
	settings *RepoSettings

	// rescanMu serialises concurrent Rescan calls so the same .db cannot
	// be opened twice in a race. Independent of mu — Rescan reads m.repos
	// via Get/Set, which take mu themselves.
	rescanMu sync.Mutex

	// inflightMu guards creating and creatingOrigins — the sets of repo names
	// and origin URLs currently being brought into the active map by a Create or
	// Restore. They are the mutual-exclusion gate that keeps two concurrent
	// operations from racing on the same name (→ duplicate registration) or the
	// same origin (→ two active repos sharing one remote).
	inflightMu      sync.Mutex
	creating        map[string]struct{}
	creatingOrigins map[string]struct{}
}

// ResolveAuth resolves a transport.AuthMethod for the given config and remote
// URL, using the manager's own key path as the SSH key fallback. It is the
// clone boundary for the immediate-clone paths (repo create and origin-session
// test), so it also enforces the local-origin policy here: an origin that the
// LocalOriginRoot gate rejects fails before any clone is attempted. Deferred
// clones (sync of a stored remote) are gated at write time via
// ValidateLocalOrigin instead.
func (m *Manager) ResolveAuth(cfg config.RemoteAuthConfig, url string) (transport.AuthMethod, error) {
	if err := m.ValidateLocalOrigin(url); err != nil {
		return nil, err
	}
	// Per-request auth configs (authConfigFromSpec) carry only the credential,
	// so inherit the operator's known_hosts location — otherwise a spec-driven
	// SSH clone would pin host keys to a different file than the sync loop.
	if cfg.KnownHosts == "" {
		cfg.KnownHosts = m.deps.Cfg.Remote.KnownHosts
	}
	return resolveAuthWithOrigin(cfg, m.deps.KeyPath, url)
}

// New returns an uninitialised Manager. Call Boot to open repos.
func New(ctx context.Context, deps Deps) *Manager {
	return &Manager{
		repos:           make(map[string]*RepoInstance),
		ctx:             ctx,
		deps:            deps,
		creating:        make(map[string]struct{}),
		creatingOrigins: make(map[string]struct{}),
	}
}

// ErrReplicaInLens rejects a lens mounting two replicas (same root-commit ID)
// of one repo: duplicated results, version confusion, ambiguous ID routing
// (RFC decision 18).
var ErrReplicaInLens = errors.New("lens mounts two replicas of the same repo")

// ErrLensBranchUnknown rejects a lens read pinned to a branch its member repo
// does not have. Failing at create beats mysteriously empty federated reads.
var ErrLensBranchUnknown = errors.New("lens pins an unknown branch")

// ErrInvalidLensName rejects a lens name that is empty or uses characters
// outside the repo-name alphabet ([a-z0-9_-]). Lens and repo names share one
// grammar so the two endpoint namespaces stay interchangeable and legible.
var ErrInvalidLensName = errors.New("invalid lens name")

// ErrLensNameConflictsRepo rejects a lens whose name equals an existing repo
// name. A lens and a lens-of-one repo both surface Binding.Name() as their
// cursor-pinning identity (RFC §7.3); if a lens and a repo shared a name a
// cursor minted on one endpoint could resume on the other. Disjoint names
// keep the binding pin sound (closes ledger gotcha M-1 /
// kb/gotchas/lens/cursor-binding-pin).
var ErrLensNameConflictsRepo = errors.New("lens name conflicts with an existing repo name")

// ValidateLens checks a lens definition against the live repo set: every
// member resolves, no two distinct members share a repo ID (decision 18), and
// every explicitly pinned branch exists in its member repo. It does not touch
// the registry. It takes m.mu.RLock for the membership snapshot; CreateLens
// uses validateLensLocked directly under its write lock instead.
func (m *Manager) ValidateLens(ctx context.Context, l Lens) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.validateLensLocked(ctx, l)
}

// validateLensLocked is ValidateLens's lock-free core: the caller must already
// hold m.mu (read or write). It reads m.repos directly — NOT via m.Get, whose
// RLock would deadlock under CreateLens's write lock (sync.RWMutex is not
// reentrant). The per-repo reads it does (ri.ID / ri.WithRead) take only
// repo-level locks, never m.mu, so they are safe to call while m.mu is held.
func (m *Manager) validateLensLocked(ctx context.Context, l Lens) error {
	// Name checks fail fast, before any member resolution: a lens name must be a
	// valid repo-grammar name and must not collide with an existing repo name,
	// so lens and repo cursor-binding namespaces stay disjoint (gotcha M-1).
	if !isValidRepoName(l.Name) {
		return fmt.Errorf("%w: %q", ErrInvalidLensName, l.Name)
	}
	// An empty write repo would otherwise flow into member resolution as
	// m.repos[""] → nil → ErrRepoNotFound ("repo not found: \"\""), masking the
	// real cause and mapping to 422. Fail fast with the specific sentinel the
	// REST layer maps to 400 (A1); the registry's own guard is now unreachable
	// through CreateLens, but stays as defence in depth.
	if l.Write == "" {
		return ErrLensWriteEmpty
	}
	if m.repos[l.Name] != nil {
		return fmt.Errorf("%w: %q", ErrLensNameConflictsRepo, l.Name)
	}
	// Collapse to one entry per member name; the write repo is implicitly a
	// member. An explicit branch pin wins over the empty (agent) default so a
	// duplicate row can't hide a bad pin.
	branches := map[string]string{l.Write: ""}
	for _, lr := range l.Reads {
		if b, ok := branches[lr.Repo]; !ok || b == "" {
			branches[lr.Repo] = lr.Branch
		}
	}
	// Resolve every member to its repo ID first, then reject any 12-hex prefix
	// collision (below) before validating branches.
	ids := make(map[string]string, len(branches)) // member name → full repo ID
	ris := make(map[string]*RepoInstance, len(branches))
	for name := range branches {
		ri := m.repos[name]
		if ri == nil {
			return fmt.Errorf("%w: %q", ErrRepoNotFound, name)
		}
		id := ri.ID()
		if id == "" {
			return fmt.Errorf("repo %q has no resolvable ID", name)
		}
		ids[name] = id
		ris[name] = ri
	}
	if err := checkMemberIDCollision(ids); err != nil {
		return err
	}
	for name, branch := range branches {
		if branch == "" {
			continue // agent-branch default, always valid
		}
		// Classify the lookup outcome: a genuinely-missing branch is the caller's
		// bad lens spec (ErrLensBranchUnknown → 4xx), but a lookup that fails for
		// any OTHER reason (ctx cancellation, transient store error) must NOT be
		// conflated with it — that would blame the caller for our failure. The
		// store preserves the distinction via store.ErrBranchNotFound (which wraps
		// plumbing.ErrReferenceNotFound); everything else propagates as-is so the
		// web layer's default arm maps it to 500, not 422.
		var lookupErr error
		ris[name].WithRead(func(svc *store.Service) {
			if svc == nil {
				lookupErr = fmt.Errorf("repo %q: store unavailable", name)
				return
			}
			_, lookupErr = svc.Branches().HeadCommit(ctx, branch)
		})
		switch {
		case lookupErr == nil:
			// Branch resolves — pin is valid.
		case errors.Is(lookupErr, store.ErrBranchNotFound):
			return fmt.Errorf("%w: %q in repo %q", ErrLensBranchUnknown, branch, name)
		default:
			return fmt.Errorf("validateLens: branch %q in repo %q: %w", branch, name, lookupErr)
		}
	}
	return nil
}

// checkMemberIDCollision rejects a lens whose members collide on the 12-hex
// routing prefix Binding.ByID uses (RFC §6.1): two members sharing that prefix
// would be misrouted, so dedup on the prefix rather than the full ID. A true
// replica shares its full ID and therefore its prefix too, so this one check
// covers both cases and keeps returning ErrReplicaInLens. ids maps member name
// → full repo ID; names are sorted so the error names the pair deterministically.
func checkMemberIDCollision(ids map[string]string) error {
	names := make([]string, 0, len(ids))
	for name := range ids {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]string, len(ids)) // 12-hex prefix → member name
	for _, name := range names {
		id := ids[name]
		prefix := id
		if len(id) >= 12 {
			prefix = id[:12]
		}
		if prev, dup := seen[prefix]; dup {
			return fmt.Errorf("%w: %q and %q share ID %s", ErrReplicaInLens, prev, name, prefix)
		}
		seen[prefix] = name
	}
	return nil
}

// CreateLens validates the definition against the live repo set, then
// persists it. ALL lens creation must go through here — LensRegistry.Create
// alone skips replica and branch validation.
//
// Two overlapping guards close the create-time races (PR-13 review 4):
//
//   - P1 (no dangling member): validation and reg.Create run under m.mu, and
//     Archive checks RefsRepo + removes the repo under the same m.mu, so the two
//     are serialized. Either Archive runs first (member gone → validation fails
//     with ErrRepoNotFound) or the lens persists first (Archive's RefsRepo then
//     sees the ref → ErrRepoInUseByLens). A member can never be archived between
//     the membership check and the persist.
//   - P2 (no repo/lens name clash): the lens name is reserved in the SAME
//     in-flight set repo Create reserves into (m.creating, via
//     reserveNameAndOrigin), so the two ops are mutually excluded on the name.
//     Racing: whichever reserves first wins; the other gets ErrCreateInFlight
//     before it can persist. Sequential: the winner releases only after
//     persisting (m.Add for a repo, reg.Create here for a lens), so the loser's
//     reservation-then-recheck observes the winner — a later repo Create sees the
//     lens via lensNameConflict, a later lens sees the repo via the m.repos check
//     under m.mu. Either way at least one side observes the other, so a repo and
//     a lens with the same name can never both persist. (A lock-free m.repos or
//     registry check alone would not: the repo side's registry re-check can slip
//     in just before this reg.Create, and m.Add does not re-check the registry.)
func (m *Manager) CreateLens(ctx context.Context, l Lens) (Lens, error) {
	// Grammar and write-empty are pure input checks; do them before reserving so
	// a malformed request never occupies a name slot.
	if !isValidRepoName(l.Name) {
		return Lens{}, fmt.Errorf("%w: %q", ErrInvalidLensName, l.Name)
	}
	if l.Write == "" {
		return Lens{}, ErrLensWriteEmpty
	}
	if len(l.Description) > MaxLensDescriptionBytes {
		return Lens{}, fmt.Errorf("%w: %d bytes (max %d)", ErrLensDescriptionTooLong, len(l.Description), MaxLensDescriptionBytes)
	}

	// Reserve the name in repo Create's in-flight set (origin empty → name only),
	// giving P2 its repo/lens mutual exclusion. release runs after m.mu.Unlock.
	release, err := m.reserveNameAndOrigin(l.Name, "")
	if err != nil {
		return Lens{}, err // ErrCreateInFlight when a create already holds this name
	}
	defer release()

	// Hold the write lock across membership validation + persist for P1.
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLensLocked(ctx, l); err != nil {
		return Lens{}, err
	}
	if m.registry == nil {
		return Lens{}, fmt.Errorf("lens registry not open")
	}
	return m.registry.Create(l)
}

// UpdateLens re-validates an edited lens definition against the live repo set,
// then persists it via LensRegistry.Update. It mirrors CreateLens's concurrency
// discipline for the same reason (P1, no dangling member): membership validation
// and the persist run under a single m.mu.Lock, and Archive checks RefsRepo +
// removes the repo under the SAME m.mu, so the two are serialized. Either Archive
// runs first (a newly-added member is gone → ErrRepoNotFound) or this update
// persists first (Archive's RefsRepo then sees the new mount → ErrRepoInUseByLens).
// A member can never be archived between the membership check and the persist.
//
// Unlike CreateLens it does NOT reserve the name in m.creating: the lens already
// exists and its name is immutable, so there is no new repo/lens name to race
// (P2). A repo Create for the lens's name still loses to the existing lens via
// its own registry re-check, independent of this call.
//
// The write repo and description are pure input, checked up front. The name is
// re-validated (grammar) but never changed — the caller passes the existing name.
func (m *Manager) UpdateLens(ctx context.Context, l Lens) (Lens, error) {
	if !isValidRepoName(l.Name) {
		return Lens{}, fmt.Errorf("%w: %q", ErrInvalidLensName, l.Name)
	}
	if l.Write == "" {
		return Lens{}, ErrLensWriteEmpty
	}
	if len(l.Description) > MaxLensDescriptionBytes {
		return Lens{}, fmt.Errorf("%w: %d bytes (max %d)", ErrLensDescriptionTooLong, len(l.Description), MaxLensDescriptionBytes)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLensLocked(ctx, l); err != nil {
		return Lens{}, err
	}
	if m.registry == nil {
		return Lens{}, fmt.Errorf("lens registry not open")
	}
	return m.registry.Update(l)
}

// Registry returns the lens registry, or nil before Start.
func (m *Manager) Registry() *LensRegistry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.registry
}

// Settings returns the per-repo settings store, or nil before Start.
func (m *Manager) Settings() *RepoSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// Get returns the RepoInstance for name, or nil if not found.
func (m *Manager) Get(name string) *RepoInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.repos[name]
}

// Set registers a RepoInstance under the given name.
func (m *Manager) Set(name string, ri *RepoInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repos[name] = ri
}

// ForEach calls fn for every registered repo while holding a read lock.
func (m *Manager) ForEach(fn func(name string, ri *RepoInstance)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, ri := range m.repos {
		fn(name, ri)
	}
}

// Names returns a sorted snapshot of registered repo names.
func (m *Manager) Names() []string {
	m.mu.RLock()
	names := make([]string, 0, len(m.repos))
	for name := range m.repos {
		names = append(names, name)
	}
	m.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Close gracefully stops all registered repositories and any background
// goroutines Start launched (currently the cluster-cache warmer).
//
// Two-pass repo shutdown: cancel all sync loops first so they wind down
// concurrently, then wait and release resources repo by repo. Returns nil
// today; the error return matches io.Closer for forward compatibility.
func (m *Manager) Close() error {
	if m.sessionReaperStop != nil {
		m.sessionReaperStop()
		m.sessionReaperStop = nil
	}

	m.mu.Lock()
	reg := m.registry
	m.registry = nil
	set := m.settings
	m.settings = nil
	m.mu.Unlock()
	if reg != nil {
		_ = reg.Close()
	}
	if set != nil {
		_ = set.Close()
	}

	m.mu.RLock()
	instances := make([]*RepoInstance, 0, len(m.repos))
	for _, ri := range m.repos {
		instances = append(instances, ri)
	}
	m.mu.RUnlock()

	// Pass 1: cancel each repo's background index heal AND sync loop so they can
	// wind down concurrently.
	for _, ri := range instances {
		ri.mu.RLock()
		cancel := ri.syncCancel
		indexCancel := ri.indexCancel
		ri.mu.RUnlock()
		if indexCancel != nil {
			indexCancel()
		}
		if cancel != nil {
			cancel()
		}
	}

	// Pass 2: wait for the heal (which may have started the loop via activate)
	// then the loop to finish — indexWg before syncWg — then shut down each
	// repo's resources. Both waits must precede closeFn so no in-flight index or
	// reconcile SQL races the SQLite handle closing.
	for _, ri := range instances {
		if ri.indexWg != nil {
			ri.indexWg.Wait()
		}
		if ri.syncWg != nil {
			ri.syncWg.Wait()
		}
		if ri.hub != nil {
			ri.hub.Shutdown()
		}
		if ri.closeFn != nil {
			ri.closeFn()
		}
	}
	return nil
}

// Start opens all repositories under cfg.Home/repos/ and launches the
// background cluster-cache warmer. core.db is opened first; remaining
// *.db files are discovered and opened. The warmer's behaviour comes
// from m.deps.Cfg.ClusterCache; check_interval=0 disables it. Callers
// must pair Start with a Close.
func (m *Manager) Start() error {
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	reg, err := OpenLensRegistry(filepath.Join(m.deps.Cfg.Home, "control.db"))
	if err != nil {
		return fmt.Errorf("open control db: %w", err)
	}
	set, err := OpenRepoSettings(filepath.Join(m.deps.Cfg.Home, "control.db"))
	if err != nil {
		// reg is not yet stored in m.registry, so Close could not reclaim
		// it — release the handle here (database/sql does not close on GC).
		_ = reg.Close()
		return fmt.Errorf("open repo settings: %w", err)
	}
	m.mu.Lock()
	m.registry = reg
	m.settings = set
	m.mu.Unlock()

	// Open the default repo with isDefault=true so that initDefaultGit is
	// called on first run (no git data in a fresh DB).
	defaultDB := filepath.Join(reposDir, config.DefaultRepoName+".db")
	ri, err := m.openOne(config.DefaultRepoName, defaultDB, true)
	if err != nil {
		return fmt.Errorf("open default repo: %w", err)
	}
	m.Set(config.DefaultRepoName, ri)

	dbFiles, _ := filepath.Glob(filepath.Join(reposDir, "*.db"))
	sort.Strings(dbFiles)
	for _, dbPath := range dbFiles {
		base := filepath.Base(dbPath)
		if store.IsSessionDBFile(base) {
			continue // ephemeral session sidecar, not a repo
		}
		name := strings.TrimSuffix(base, ".db")
		if name == config.DefaultRepoName {
			continue
		}
		if !isValidRepoName(name) {
			log.Warn().Str("file", base).Msg("skipping db with invalid repo name")
			continue
		}
		if err := m.Add(name, dbPath); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("skipping repo")
		}
	}

	// Launch the background idle-session reaper. A misconfigured session block
	// surfaces at boot rather than silently disabling the reaper.
	reaperCfg, err := parseSessionReaperConfig(m.deps.Cfg.Session)
	if err != nil {
		return fmt.Errorf("session reaper config: %w", err)
	}
	m.sessionReaperStop = m.startSessionReaper(reaperCfg)
	return nil
}

// Add opens a single repository and registers it under name.
// Each repo loads its own ontology from its git store during initialization.
//
// Add deliberately does NOT enforce ErrRepoNameConflictsLens (the reverse M-1
// guard). Add registers repos that already exist on disk — the Start/Rescan
// discovery loops and the recovery paths inside Archive/Restore all go through
// here — so refusing a lens-name collision would DROP a repo whose collision
// predates this fix (or was created out-of-band), silently unregistering real
// data. The invariant is enforced loud at the user-facing creation boundary
// (CreatePreflight/Create/Restore) and soft at startup: an already-existing
// collision keeps its repo, and operators resolve it by renaming the lens.
func (m *Manager) Add(name, dbPath string) error {
	ri, err := m.openOne(name, dbPath, false)
	if err != nil {
		return err
	}
	m.Set(name, ri)
	return nil
}

// Rescan re-discovers repos under <home>/repos/. Any *.db file whose name
// matches isValidRepoName and is not already registered is opened via Add.
// Already-registered repos are reported in Skipped and otherwise untouched.
//
// This is the runtime counterpart of the discovery loop inside Start: it
// lets a running server pick up new repos created by `knomit init` without
// a restart. Removed or replaced .db files are NOT handled — see the
// design doc for the rationale.
//
// Concurrent calls are serialised by rescanMu. On success the returned
// slices are always non-nil (possibly empty). The error return is non-nil
// only when the repos directory cannot be read; in that case the returned
// RescanResult is the zero value. Per-repo Add failures appear in
// result.Errors and do not abort the scan.
func (m *Manager) Rescan() (RescanResult, error) {
	m.rescanMu.Lock()
	defer m.rescanMu.Unlock()

	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if _, err := os.Stat(reposDir); err != nil {
		return RescanResult{}, fmt.Errorf("stat repos dir: %w", err)
	}

	dbFiles, err := filepath.Glob(filepath.Join(reposDir, "*.db"))
	if err != nil {
		// filepath.Glob can only return ErrBadPattern, which is unreachable
		// for our literal pattern — but keep the guard for forward safety.
		return RescanResult{}, fmt.Errorf("glob repos dir: %w", err)
	}
	sort.Strings(dbFiles)

	result := RescanResult{
		Added:   []string{},
		Skipped: []string{},
		Errors:  []RescanError{},
	}

	for _, dbPath := range dbFiles {
		base := filepath.Base(dbPath)
		if store.IsSessionDBFile(base) {
			continue // ephemeral session sidecar, not a repo
		}
		name := strings.TrimSuffix(base, ".db")
		if !isValidRepoName(name) {
			continue
		}
		// A Create/Restore in flight has already put the .db on disk but not yet
		// registered the name (the whole clone happens in that window). Opening
		// the file here would double-open the same database and orphan one
		// instance's handle and goroutines when the create's Add overwrites the
		// map entry — so honour the same reservation gate Create/Restore hold.
		// Check the reservation BEFORE the map: the reservation is released only
		// after Add, so a name missing from both really is unowned.
		if m.isCreateInFlight(name) {
			result.Skipped = append(result.Skipped, name)
			continue
		}
		if m.Get(name) != nil {
			result.Skipped = append(result.Skipped, name)
			continue
		}
		if err := m.Add(name, dbPath); err != nil {
			result.Errors = append(result.Errors, RescanError{Repo: name, Err: err})
			log.Warn().Err(err).Str("repo", name).Msg("rescan: add failed")
			continue
		}
		result.Added = append(result.Added, name)
		log.Info().Str("repo", name).Msg("rescan: opened")
	}
	return result, nil
}

// isCreateInFlight reports whether a Create/Restore currently holds the
// reservation for name (see reserveNameAndOrigin).
func (m *Manager) isCreateInFlight(name string) bool {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	_, ok := m.creating[name]
	return ok
}

// ---------- private helpers ----------

// openOne initialises a single repo from a SQLite database file.
// If isDefault is true and no git data exists, the repo is initialised
// from scratch (or cloned from origin). Non-default repos that fail to
// open are returned as errors so the caller can skip them gracefully.
func (m *Manager) openOne(name, dbPath string, isDefault bool) (*RepoInstance, error) {
	b := repoBuilder{
		name:                  name,
		dbPath:                dbPath,
		isDefault:             isDefault,
		cfg:                   m.deps.Cfg,
		signer:                m.deps.Signer,
		agentBranch:           m.deps.AgentBranch,
		embedder:              m.deps.Embedder,
		keyPath:               m.deps.KeyPath,
		ctx:                   m.ctx,
		disableBackgroundSync: m.deps.DisableBackgroundSync,
	}

	if err := b.openStore(); err != nil {
		return nil, err
	}
	if err := b.openGit(); err != nil {
		b.close()
		return nil, err
	}
	b.loadOntology()
	b.ensureBranch()
	b.setupIndex()
	b.seedWatermarks()

	ri := b.build()

	// Synchronous open for test harnesses (DisableBackgroundSync): build the
	// index and activate inline so the index is ready when openOne returns —
	// preserving the open→index-ready contract many tests rely on.
	if b.disableBackgroundSync {
		ok := healIndexBranches(b.ctx, b.svc.IndexManager(), b.name, b.indexBranches, b.indexStale, nil)
		b.activate()
		if ok {
			ri.markIndexReady()
		} else {
			ri.markIndexFailed()
		}
		return ri, nil
	}

	// Production: the heavy initial index runs in the BACKGROUND. The store is
	// already live, so the HTTP server / UI come up immediately and reads work
	// progressively (partial until "ready"). The remote sync loops start only
	// after indexing (b.activate). The heal itself holds lockBranch per branch
	// (Rebuild self-locks; the incremental path uses SyncLocked), so a
	// concurrent inline write or the live commit observer (which also uses
	// SyncLocked) is serialized with it rather than racing the index watermark.
	//
	// The heal watches b.indexCtx — its OWN context, NOT syncCtx. syncCtx is
	// cancelled by startSync (ActivateSync) to restart the reconcile loop; a
	// runtime clone-create calls ActivateSync right after this Add, so sharing
	// syncCtx would cancel the in-flight heal and pin the index at "indexing"
	// forever (the very bug this split fixes). indexCtx is cancelled only by a
	// real teardown (shutdown/Close/SwapStore via ri.indexCancel, or b.ctx), so
	// a close mid-index aborts the heal and skips activation.
	//
	// The heal goroutine is registered with b.indexWg so every teardown path
	// (Manager.Close, Archive→shutdown, SwapStore) — each of which does
	// indexWg.Wait() BEFORE svc.Close() — waits for the heal to finish before
	// the SQLite handle is closed. Without this the close would race in-flight
	// index SQL on the same *sql.DB ("database is closed"). The Add happens
	// here (synchronously, before openOne returns), so it is ordered before any
	// teardown Wait; b.activate's own syncWg.Add runs while indexWg is still
	// held, and teardown waits indexWg before syncWg, so the loop counter never
	// transiently reads zero.
	ri.markIndexing()
	b.indexWg.Add(1)
	go func() {
		defer b.indexWg.Done()
		progress := func(_ string, done, total int) { ri.setIndexProgress(done, total) }
		ok := healIndexBranches(b.indexCtx, b.svc.IndexManager(), b.name, b.indexBranches, b.indexStale, progress)
		if b.indexCtx.Err() != nil {
			// Repo was closed/cancelled mid-index — a clean shutdown, not a
			// failure. Skip activation and leave the state as-is; the instance
			// is being torn down and its status is no longer observed.
			return
		}
		// Activate the sync loops even on a failed heal so the reconcile/push
		// loops can retry and recover; the index state still reflects that the
		// initial heal did not fully complete.
		b.activate()
		if ok {
			ri.markIndexReady()
		} else {
			ri.markIndexFailed()
		}
	}()

	return ri, nil
}

// IsValidName reports whether s satisfies the repo/lens name grammar
// (lowercase letters, digits, hyphens, or underscores, non-empty). It is a
// thin exported wrapper over isValidRepoName so external callers (e.g. the
// bridge's `claude init`) can validate names against the single source of
// truth without duplicating the grammar.
func IsValidName(s string) bool {
	return isValidRepoName(s)
}

// isValidRepoName checks that a repo name contains only lowercase letters,
// digits, hyphens, or underscores.
func isValidRepoName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
