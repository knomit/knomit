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
	"time"

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
//   - Errors: per-repo Add failures, plus repos refused by the credential gate
//     (see gateCredential — a refused repo is opened, then taken back out of
//     service, so it is neither Added nor Skipped); other repos still attempted.
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

	// repoRegistry is the authoritative repo registry (third tenant of
	// <home>/control.db) — the answer to "what repos should exist?" that the
	// filesystem cannot give on an empty disk. Opened by Start, closed by
	// Close; nil before Start.
	repoRegistry *RepoRegistry

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

// RepoRegistry returns the authoritative repo registry, or nil before Start.
//
// Named RepoRegistry rather than Registry because Registry() was already taken
// by the LENS registry, which predates this one and has callers across the
// package and the web layer.
func (m *Manager) RepoRegistry() *RepoRegistry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.repoRegistry
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

// unregister removes a name from the active map WITHOUT touching the registry
// row or the database file — the repo stops being served, and stays on disk and
// on the books so the next boot can retry it.
//
// Tearing the instance down is the caller's job: this only makes it
// unreachable, so a caller that drops the last reference must have called
// shutdown itself or it leaks the SQLite handle.
func (m *Manager) unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.repos, name)
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
	rr := m.repoRegistry
	m.repoRegistry = nil
	m.mu.Unlock()
	if reg != nil {
		_ = reg.Close()
	}
	if set != nil {
		_ = set.Close()
	}
	if rr != nil {
		_ = rr.Close()
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

// Start opens every repo the control.db registry says should exist and
// launches the background idle-session reaper. Callers must pair Start with a
// Close.
//
// Zero repos is a valid outcome, and on a fresh home it is the ONLY outcome:
// knomit has no default repo, creates nothing implicitly, and comes up serving
// an empty repo collection until the user creates one. Every repo without
// exception arrives through the reconcile loop below, which is what makes
// recovery uniform.
//
// The registry — NOT a repos/*.db glob — is authoritative. A glob answers "what
// is on this disk", which is the empty set on a machine whose volume was
// replaced, so the old discovery silently came up with no repos at all and no
// record anywhere of what had been lost. The registry answers "what SHOULD
// exist", and each row is reconciled against the disk: present → open, absent
// but with a recorded origin → re-clone, absent with nothing to rebuild from →
// omitted.
//
// No single repo can fail the boot. Every per-row outcome above — an open that
// failed, a clone that failed, nothing to rebuild from — is logged at ERROR and
// skipped, so one repo's unreachable origin or corrupt database cannot take the
// other repos on the instance offline with it. The registry row survives in
// every case, so the next restart retries. Start returns an error only for
// conditions that affect the whole manager: the repos directory, control.db,
// and the session reaper's configuration.
func (m *Manager) Start() error {
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	controlDB := filepath.Join(m.deps.Cfg.Home, "control.db")
	reg, err := OpenLensRegistry(controlDB)
	if err != nil {
		return fmt.Errorf("open control db: %w", err)
	}
	set, err := OpenRepoSettings(controlDB)
	if err != nil {
		// reg is not yet stored in m.registry, so Close could not reclaim
		// it — release the handle here (database/sql does not close on GC).
		_ = reg.Close()
		return fmt.Errorf("open repo settings: %w", err)
	}
	repoReg, err := OpenRepoRegistry(controlDB)
	if err != nil {
		_ = reg.Close()
		_ = set.Close()
		return fmt.Errorf("open repo registry: %w", err)
	}

	// Supply the same key material the repo stores use. NewCrypt derives its
	// AES key from the agent SSH key with a fixed HKDF info string and no
	// per-repo salt, so a token encrypted by a repo's crypt decrypts with this
	// one — the boot migration is a move, not a re-key.
	//
	// Warn rather than fail: a missing key must not take the server down. It
	// costs the ability to store or read credentials, which SetOriginCredential
	// and OriginCredential each report at their own call site.
	if keyData, kerr := os.ReadFile(m.deps.KeyPath); kerr != nil {
		log.Warn().Err(kerr).Str("key_path", m.deps.KeyPath).
			Msg("registry credential encryption unavailable: agent key unreadable")
	} else if crypt, cerr := store.NewCrypt(keyData); cerr != nil {
		log.Warn().Err(cerr).Msg("registry credential encryption unavailable: cannot derive key")
	} else {
		repoReg.SetCrypt(crypt)
	}

	// Stored before the first fallible step below, so every later return path
	// leaves the handles reclaimable by Close.
	m.mu.Lock()
	m.registry = reg
	m.settings = set
	m.repoRegistry = repoReg
	m.mu.Unlock()

	// Adopt BEFORE opening anything: on an upgrade the registry is empty and
	// the disk is the only record of what exists, so the filesystem gets one
	// last say. Afterwards the registry is populated and never consults the
	// disk again.
	if _, err := adoptFromFilesystem(repoReg, reposDir); err != nil {
		return fmt.Errorf("adopt repos: %w", err)
	}

	records, err := repoReg.List(RepoActive)
	if err != nil {
		return fmt.Errorf("list registered repos: %w", err)
	}

	for _, rec := range records {
		// The name becomes a path below, so re-validate it here rather than
		// trusting the row. Everything that writes the registry validates
		// first; this guards a hand-edited or corrupted control.db.
		if !isValidRepoName(rec.Name) {
			log.Error().Str("repo", rec.Name).Msg("registry row has an invalid repo name; skipping")
			continue
		}
		dbPath := filepath.Join(reposDir, rec.Name+".db")
		if _, statErr := os.Stat(dbPath); statErr == nil {
			if err := m.Add(rec.Name, dbPath); err != nil {
				// ERROR, not WARN: the repo disappears from the API entirely,
				// so this line is the ONLY signal the user gets, and it has to
				// carry the recovery the same way the re-clone branch below
				// does. This is a TERMINAL state, not a transient one: every
				// later boot repeats it verbatim, and unlike a missing database
				// there is no origin path that might heal it on its own.
				//
				// The realistic cause is a database that exists but holds no
				// git data — an install interrupted midway through creating the
				// repo, back when the default repo was bootstrapped implicitly
				// at boot. Such a file has nothing in it worth keeping, but
				// only the operator can say that, so name it and stop.
				log.Error().Err(err).Str("repo", rec.Name).Str("db", dbPath).
					Msgf("repo %q failed to open and was skipped; it will NOT appear in the API,"+
						" and every restart will fail the same way until this is resolved."+
						"\nEither restore %s from a backup,"+
						" or, if that database is known to be empty or corrupt, move it aside"+
						" and remove the repo from the registry with:"+
						"\n  sqlite3 %s \"DELETE FROM repos WHERE name = '%s' AND archive_id = '';\""+
						"\nIf the repo has an origin you can re-create it afterwards from the same URL;"+
						" a repo created without one has its only copy of its history in that file.",
						rec.Name,
						dbPath, filepath.Join(m.deps.Cfg.Home, "control.db"), rec.Name,
					)
				continue
			}
			// ORDERING: this is the migration the note below demands, and it
			// runs FIRST for exactly that reason. Moving it after
			// reconcileOrigin destroys credentials silently —
			// TestBootMigratesBeforeOriginReconcile is the regression that
			// catches it, and it is the ONLY test that does.
			//
			// Skipping a refused repo is the whole point: serving one whose
			// credential is unrecoverable defers the failure to the day the
			// database is lost, which is the day it cannot be fixed.
			if cerr := m.gateCredential(rec.Name, dbPath); cerr != nil {
				continue
			}
			// ORDERING: any migration that lifts a credential out of this
			// repo's store and into control.db MUST run BEFORE this line.
			// reconcileOrigin materializes with SetRemote, which is INSERT OR
			// REPLACE with EMPTY auth columns — so on any boot where control.db
			// disagrees about the URL or branch it EMPTIES the store's
			// auth_method/auth_token. For a legacy repo whose token lives only
			// in the store, that is the only copy, and it is gone permanently.
			m.reconcileOrigin(rec)
			continue
		}
		if rec.OriginURL != "" {
			// Absent but recoverable: rebuild through the ordinary
			// create-with-origin path so setupIndex syncs BOTH the agent branch
			// and upstreamMain, as the initial-upstream-index-sync invariant requires.
			//
			// A failure is logged and SKIPPED, not returned. The realistic
			// triggers are an unreachable origin host and an expired credential
			// — transient, external, and specific to ONE repo — while refusing
			// the boot takes every other healthy repo on the instance down with
			// it — one dependency's bad day becoming a whole-instance outage.
			// The registry row survives, so the next boot retries the clone by
			// itself.
			//
			// ERROR rather than WARN, with the recovery spelled out: this repo
			// is absent from the API until the clone succeeds, and this line is
			// the only signal saying why.
			if err := m.rebuildFromOrigin(rec, dbPath); err != nil {
				log.Error().Err(err).Str("repo", rec.Name).Str("db", dbPath).Str("origin", rec.OriginURL).
					Msgf("repo %q is registered but its database %s is missing, and re-cloning it from %s failed;"+
						" it will NOT appear in the API, and the next restart will try again."+
						"\nTo resolve it now, either restore %s from a backup,"+
						" or remove the repo from the registry with:"+
						"\n  sqlite3 %s \"DELETE FROM repos WHERE name = '%s' AND archive_id = '';\""+
						"\nIf the origin needs a credential, control.db holds it and the clone will use it;"+
						" a failure here is most likely the origin being unreachable or the credential"+
						" no longer being accepted.",
						rec.Name, dbPath, rec.OriginURL,
						dbPath, filepath.Join(m.deps.Cfg.Home, "control.db"), rec.Name,
					)
			}
			continue
		}
		// No database and no origin to rebuild from — the one genuinely
		// unrecoverable case, since a repo created without an origin keeps its
		// only copy of its git history inside that .db. Logged at ERROR and
		// skipped rather than refusing the boot: one unrecoverable repo must not
		// take the whole server down with it.
		log.Error().Str("repo", rec.Name).
			Msg("registered repo has no database and no origin to rebuild from; it will not appear in the API")
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

// RecordOrigin copies a repo's live origin remote from its store into its
// registry row.
//
// # Every origin change must reach control.db, and this is how
//
// The store's `remotes` table is the source of truth for the remote; the
// registry row is a copy of it kept for the one case the store cannot serve —
// the store file is GONE, and Manager.Start needs to know where to re-clone
// from. A copy is only worth having if it is kept current, so EVERY path that
// changes a repo's origin calls this afterwards: clone-mode Create (via the
// row it writes directly), the boot-time reconcile in Start, Rescan's adoption
// of an out-of-band database, and the three web surfaces that let a user
// connect, re-point or disconnect an origin. Without the last of those, an
// origin attached after creation lived only in the store, and a volume loss
// that also lost the replica left the repo registered with nothing to rebuild
// from — the exact hole the registry exists to close.
//
// It is AUTHORITATIVE, which means it CLEARS the registry's origin when the
// store reports none. A disconnect is an origin change like any other, and a
// registry that kept the old URL would have the next boot re-clone from a
// remote the user deliberately removed.
//
// It writes nothing when the store could not be READ (originOf's ok=false) —
// see there for why the two empties are not the same thing — and nothing when
// the row is already correct, so it is cheap enough to call unconditionally.
//
// Failures are logged, never returned. Callers reach here after the store
// mutation has already committed, so failing them would report an error for a
// change that visibly succeeded; the message says plainly what the stale row
// costs.
func (m *Manager) RecordOrigin(name string) {
	reg := m.RepoRegistry()
	if reg == nil {
		return
	}
	url, branch, ok := originOf(m.Get(name))
	if !ok {
		return // store unreadable: nothing learned, so leave the registry alone
	}
	rec, found, err := reg.ActiveRecord(name)
	if err != nil {
		log.Warn().Err(err).Str("repo", name).Msg("origin write-through: read registry failed")
		return
	}
	if !found || (rec.OriginURL == url && rec.OriginBranch == branch) {
		return
	}
	rec.OriginURL = url
	rec.OriginBranch = branch
	if err := reg.Upsert(rec); err != nil {
		log.Error().Err(err).Str("repo", name).Str("origin", url).
			Msg("origin write-through: registry write failed; control.db still holds the OLD origin, so a rebuild " +
				"after this repo's database is lost would clone from the wrong remote (or none)")
	}
}

// reconcileOrigin brings a repo's store and its control.db row into agreement
// at boot, with control.db as the authority for an origin it actually knows.
//
// # Why not simply "control.db always wins"
//
// Because "control.db has no origin" is ambiguous, and getting it wrong destroys
// data. adoptFromFilesystem writes rows with an empty OriginURL — it never opens
// the stores, so it cannot know their origins — which means a blank row means
// EITHER "the user disconnected this origin" OR "control.db has not learned it
// yet". Unwiring on a blank row would strip a working origin off every repo that
// predates the registry.
//
// So the ambiguity is resolved by severity, and there are exactly two rules:
//
//   - control.db knows an origin and the store disagrees -> push it DOWN. This
//     repairs a crash between SetOrigin's two writes, which is the case that
//     otherwise leaves a credential recorded against no URL and a repo that can
//     never be rebuilt.
//   - control.db knows none and the store has one -> pull it UP (RecordOrigin,
//     unchanged). Legacy and adopted rows learn their origin instead of losing it.
//
// # A blank BRANCH on a known URL is left blank, deliberately
//
// There is no third rule teaching control.db a branch from the store, and the
// absence is the point.
//
// A blank branch is the SAFE state, not a gap. rebuildFromOrigin passes
// rec.OriginBranch straight into OriginSpec.Branch, and initClone calls
// detectUpstream only when that branch is EMPTY — so a blank row re-detects the
// remote's real default on every rebuild. It is self-correcting.
//
// A branch adopted at boot is not. store.SetRemote hardcodes "main" when handed
// an empty branch; it never asks the remote. So a branch read back out of the
// store may be a substitution rather than a detection, and recording it PINS it:
// the next rebuild clones with Branch:"main" explicitly, which bypasses
// detectUpstream — the one mechanism that would have found "master". The wrong
// pin permanently disables its own correction. That is strictly worse than blank.
//
// Nothing is lost by omitting the rule, because control.db already learns the
// detected branch where it is genuinely detected: Create calls RecordOrigin after
// initClone has resolved the upstream (see lifecycle.go), and legacy or adopted
// rows learn theirs through rule two's RecordOrigin.
//
// For the same reason the materialize above never hands SetRemote an empty
// branch while the store still has one — see there.
//
// # The residual, stated plainly
//
// Rule two means a crash midway through ClearOrigin can RESURRECT a disconnected
// origin: control.db was cleared first, the store is still wired, and this cannot
// tell that apart from a row that never knew. That trade is deliberate —
// resurrection costs the user a second disconnect, while unwiring a legacy repo
// costs them a working remote. Do not "fix" it by unwiring blank rows.
//
// Best-effort and logged: a repo that is open and serving must not be taken down
// because its origin bookkeeping disagreed.
func (m *Manager) reconcileOrigin(rec RepoRecord) {
	ri := m.Get(rec.Name)
	if ri == nil {
		return
	}
	svc, release, err := ri.Acquire()
	if err != nil {
		log.Warn().Err(err).Str("repo", rec.Name).
			Msg("boot: origin reconcile skipped; the store could not be acquired")
		return
	}
	defer release()

	rm, gerr := svc.Remote().GetRemote("origin")
	if gerr != nil {
		// The store could not be READ, which is originOf's ok=false: nothing was
		// learned, so neither direction is safe. Writing DOWN here would guess at
		// intervals we failed to read and overwrite whatever the row really holds;
		// writing UP is what RecordOrigin already refuses to do for this reason.
		log.Warn().Err(gerr).Str("repo", rec.Name).
			Msg("boot: origin reconcile skipped; the store's remote could not be read")
		return
	}
	var storeURL, storeBranch string
	// Intervals are carried over rather than re-defaulted: a materialize-down is
	// a repair of the URL, not an invitation to reset a cadence the user chose.
	// 300/300 is the same default the clone path writes (see initClone), used
	// only when there is no row to carry anything over from.
	interval, pushInterval := 300, 300
	if rm != nil {
		storeURL, storeBranch = rm.URL, rm.Branch
		interval, pushInterval = rm.Interval, rm.PushInterval
	}

	if rec.OriginURL == "" {
		// control.db does not know an origin. Adopt whatever the store has;
		// never unwire. See the ambiguity note above.
		m.RecordOrigin(rec.Name)
		return
	}

	if storeURL == rec.OriginURL && (rec.OriginBranch == "" || storeBranch == rec.OriginBranch) {
		return // already in agreement, or agreed on everything control.db knows
	}

	// The branch handed to SetRemote is never left empty while the store still
	// has one, because SetRemote answers an empty branch with a hardcoded "main"
	// — it does not ask the remote. Carrying the store's own branch over keeps
	// this repair from INVENTING an upstream on a master-default origin. When
	// neither side has a branch there is nothing to carry and the default fires;
	// that is worth a line in the log, because it is a guess.
	branch := rec.OriginBranch
	if branch == "" {
		branch = storeBranch
		if branch == "" {
			log.Warn().Str("repo", rec.Name).Str("origin", rec.OriginURL).
				Msg("boot: materializing an origin whose upstream branch nobody has resolved; the store will " +
					"default to \"main\", which is wrong on a master-default remote. control.db's branch is " +
					"deliberately left blank so a rebuild re-detects it")
		}
	}
	if serr := svc.Remote().SetRemote("origin", rec.OriginURL, branch,
		ri.AgentBranch(), interval, pushInterval, "", ""); serr != nil {
		log.Error().Err(serr).Str("repo", rec.Name).Str("origin", rec.OriginURL).
			Msg("boot: control.db knows this repo's origin but wiring it into the store failed; " +
				"sync will not run until this is resolved")
		return
	}
	log.Info().Str("repo", rec.Name).Str("origin", rec.OriginURL).Str("branch", branch).
		Msg("boot: materialized control.db's origin into the store")

	// Nothing is written back UP from here. control.db's blank branch stays
	// blank on purpose — see the doc comment: blank re-detects on rebuild,
	// whereas a branch adopted from the store may be SetRemote's substitution
	// and pinning it would bypass detectUpstream forever.
}

// rebuildFromOrigin recreates a registered repo whose database file is absent,
// cloning from its recorded origin. It delegates to the ordinary create path so
// the clone inherits setupIndex's both-branch sync rather than reimplementing it.
//
// Create is re-entrant here: Start holds no lock across this call, and Create's
// own registry write-through upserts the SAME row (keyed by name), so the
// rebuild neither deadlocks nor duplicates.
//
// The one thing Create gets wrong for a rebuild is CreatedAt, which it stamps
// afresh — a repo that has just been recovered is not a repo that has just been
// made. That single field is restored afterwards, by reading back the row Create
// and RecordOrigin left and correcting it. Writing the ORIGINAL record back
// wholesale would be wrong: RecordOrigin has by then recorded the branch the
// clone actually resolved (detectUpstream turns an empty request into the
// remote's real default), and a whole-row write would immediately discard it in
// favour of the blank this rebuild started from.
func (m *Manager) rebuildFromOrigin(rec RepoRecord, dbPath string) error {
	log.Info().Str("repo", rec.Name).Str("origin", rec.OriginURL).Str("db", dbPath).
		Msg("registered repo missing locally; rebuilding from origin")
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := m.Create(ctx, m.rebuildSpec(rec), nil); err != nil {
		return err
	}
	m.restoreCreatedAt(rec)
	return nil
}

// rebuildSpec builds the CreateSpec rebuildFromOrigin hands to Create.
//
// The credential is the whole reason control.db can rebuild a private origin
// at all: without it a clone-mode Create can only try anonymous access, which
// fails against exactly the origins that most need rebuilding. A read
// failure is logged rather than fatal — the clone is still worth attempting
// unauthenticated, since a public origin succeeds without one, and refusing
// the rebuild entirely over an unreadable credential would take an otherwise
// recoverable repo down with it.
//
// Split out from rebuildFromOrigin so tests can assert on the exact spec
// Create receives — a live rebuild against a permissive (e.g. file://) test
// origin would clone successfully whether or not the credential made it into
// the spec, which would let a regression that drops the credential pass
// silently.
func (m *Manager) rebuildSpec(rec RepoRecord) CreateSpec {
	spec := CreateSpec{
		Name: rec.Name,
		Mode: "clone",
		Origin: &OriginSpec{
			URL:    rec.OriginURL,
			Branch: rec.OriginBranch,
		},
	}
	if reg := m.RepoRegistry(); reg != nil {
		method, token, cerr := reg.OriginCredential(rec.Name)
		if cerr != nil {
			log.Warn().Err(cerr).Str("repo", rec.Name).
				Msg("rebuild: recorded credential unreadable; attempting an unauthenticated clone")
		} else {
			spec.Origin.AuthMethod = method
			spec.Origin.AuthToken = token
		}
	}
	return spec
}

// restoreCreatedAt puts a rebuilt repo's original creation time back on the row
// Create just stamped with time.Now(), leaving every other column as the rebuild
// left it.
//
// Best-effort and logged, never returned: the repo is cloned, open and serving
// by the time this runs, so a failure here costs provenance, not availability —
// and reporting it as a failed rebuild would be a lie about the repo's state.
func (m *Manager) restoreCreatedAt(rec RepoRecord) {
	if rec.CreatedAt.IsZero() {
		return // nothing recorded to preserve
	}
	reg := m.RepoRegistry()
	if reg == nil {
		return
	}
	current, found, err := reg.ActiveRecord(rec.Name)
	if err != nil || !found {
		// Create's own write-through logs its failure; there is no row to
		// correct, and inventing one here would undo that diagnosis.
		if err != nil {
			log.Warn().Err(err).Str("repo", rec.Name).
				Msg("rebuild: could not read back the registry row; creation time not restored")
		}
		return
	}
	if current.CreatedAt.Equal(rec.CreatedAt) {
		return
	}
	current.CreatedAt = rec.CreatedAt
	if uerr := reg.Upsert(current); uerr != nil {
		log.Warn().Err(uerr).Str("repo", rec.Name).
			Msg("rebuild: repo is back but its creation time now reads as the moment it was rebuilt")
	}
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
	ri, err := m.openOne(name, dbPath)
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
// This is the runtime counterpart of the registry reconcile inside Start: it
// lets a running server pick up a .db that appeared out-of-band (a hand-copied
// file, a partial restore) without a restart. Removed or replaced .db files are NOT handled — see the
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
		// Register it. Rescan is the one path that adopts a .db which appeared
		// out-of-band (a hand-copied file, a partial restore), and Start no
		// longer globs the directory — so without this row the repo would silently
		// vanish at the next boot.
		//
		// EnsureActive, not Upsert: this name may ALREADY have a row. Start skips
		// and logs a repo whose open failed, leaving its registration behind, and
		// a rescan is exactly how such a repo comes back — so a whole-row write
		// here would blank the origin and restamp the creation time of a row that
		// was already right. See EnsureActive.
		if reg := m.RepoRegistry(); reg != nil {
			if uerr := reg.EnsureActive(name, time.Now().UTC()); uerr != nil {
				log.Error().Err(uerr).Str("repo", name).
					Msg("rescan: opened repo but failed to register it; it will not survive a restart")
			} else {
				m.RecordOrigin(name)
			}
		}
		// The credential gate applies here too, and NOT gating it was a real
		// bypass: this doc block says a rescan is exactly how a repo Start
		// skipped comes back, so an operator answering the boot's refusal with a
		// rescan would have put the repo back into service with its credential
		// still only in the store — syncing anonymously against a private
		// origin, because control.db's empty auth_token reads as "no credential"
		// rather than as an error. See gateCredential.
		//
		// It runs AFTER EnsureActive because the migration writes to the active
		// row, and reports through Errors rather than Skipped: a refusal has a
		// reason the operator needs, and Skipped is documented as "already
		// registered" and carries none.
		if cerr := m.gateCredential(name, dbPath); cerr != nil {
			result.Errors = append(result.Errors, RescanError{Repo: name, Err: cerr})
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

// openOne initialises a single repo from a SQLite database file. The database
// must already hold git data — creating one is Manager.Create's job — so a repo
// that fails to open is returned as an error for the caller to skip or report.
func (m *Manager) openOne(name, dbPath string) (*RepoInstance, error) {
	b := repoBuilder{
		name:                  name,
		dbPath:                dbPath,
		cfg:                   m.deps.Cfg,
		signer:                m.deps.Signer,
		agentBranch:           m.deps.AgentBranch,
		embedder:              m.deps.Embedder,
		keyPath:               m.deps.KeyPath,
		ctx:                   m.ctx,
		disableBackgroundSync: m.deps.DisableBackgroundSync,
		authResolve:           func() (config.RemoteAuthConfig, error) { return m.OriginAuth(name) },
	}

	if err := b.openStore(); err != nil {
		return nil, err
	}
	if err := b.openGit(); err != nil {
		b.close()
		return nil, err
	}
	// ensureBranch must run before loadOntology: on a restored/copied home the
	// configured agent branch is absent until ensureBranch adopts it (issue
	// #32), and loadOntology reads (and may rewrite) domains/ontology.yaml on
	// that branch. Running loadOntology first would fall back to the default
	// ontology and skip the preset-refresh on the first boot after a restore.
	b.ensureBranch()
	b.loadOntology()
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
