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

	// reg is the repo registry (second tenant of <home>/control.db) — the
	// authoritative record of which repos exist, including each one's serving
	// profile. Opened by Start, closed by Close; nil before Start.
	reg *Registry

	// origins holds each repo's remote connection (third tenant, sharing reg's
	// handle). Opened by Start, closed with reg — Origins has no Close of its
	// own, it borrows reg's *sql.DB; nil before Start.
	origins *Origins

	// byUID indexes the same instances as repos, keyed by registry uid. Lens
	// membership resolves through it: lenses reference uids, not names, so a
	// rename never touches a lens row.
	byUID map[string]*RepoInstance

	// unavailable holds a registered repo that has no live instance, keyed by
	// uid — the file is missing, failed to open, or its knowledge base
	// conflicts with an already-open repo. Populated by openRegistered during
	// Start (and later, rehydrate); cleared when the repo comes back. A repo
	// here is NEVER also in repos/byUID, and vice versa.
	unavailable map[string]Unavailable

	// orphanFiles are the .db base names Start found under repos/ with no
	// registry row — active or archived. Recorded rather than only logged so
	// the classification is observable: the distinction that matters is that an
	// ARCHIVED repo's file is registered, not an orphan, and only Start knows
	// which set it consulted.
	orphanFiles []string

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
		byUID:           make(map[string]*RepoInstance),
		unavailable:     make(map[string]Unavailable),
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
// name. The cursor-pinning identity (RFC §7.3) is Binding.PinID() now —
// repo:<uid> / lens:<uid> — which cannot collide between a lens and a repo
// even if they share a name, so this guard is no longer what keeps the
// binding pin sound. It survives as a UX nicety: a lens and a lens-of-one
// repo share one display-name and endpoint-path namespace, and letting them
// collide would make "which one did I mean" ambiguous in URLs, error
// messages, and logs (closes ledger gotcha M-1 / kb/gotchas/lens/cursor-binding-pin).
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
// hold m.mu (read or write). It reads m.byUID / m.repos directly — NOT via
// m.GetByUID or m.Get, whose RLock would deadlock under CreateLens's write lock
// (sync.RWMutex is not reentrant). The per-repo reads it does (ri.ID /
// ri.WithRead) take only repo-level locks, never m.mu, so they are safe to call
// while m.mu is held.
//
// Members resolve by registry uid; only the lens NAME is checked against repo
// names. The cursor-pinning identity (RFC §7.3) is Binding.PinID() now —
// repo:<uid> / lens:<uid> — which cannot collide between a lens and a repo
// even if they share a name, so this check is no longer what keeps cursor
// namespaces disjoint; it survives as a UX nicety (one name must never serve
// two endpoints).
func (m *Manager) validateLensLocked(ctx context.Context, l Lens) error {
	// Name checks fail fast, before any member resolution: a lens name must be a
	// valid repo-grammar name and must not collide with an existing repo name
	// (namespace legibility — one name must never serve two endpoints; gotcha M-1).
	if !isValidRepoName(l.Name) {
		return fmt.Errorf("%w: %q", ErrInvalidLensName, l.Name)
	}
	// An empty write uid would otherwise flow into member resolution as
	// m.byUID[""] → nil → ErrRepoNotFound ("repo not found: \"\""), masking the
	// real cause and mapping to 422. Fail fast with the specific sentinel the
	// REST layer maps to 400 (A1); the registry's own guard is now unreachable
	// through CreateLens, but stays as defence in depth.
	if l.WriteUID == "" {
		return ErrLensWriteEmpty
	}
	if m.repos[l.Name] != nil {
		return fmt.Errorf("%w: %q", ErrLensNameConflictsRepo, l.Name)
	}
	// Collapse to one entry per member uid; the write repo is implicitly a
	// member. An explicit branch pin wins over the empty (agent) default so a
	// duplicate row can't hide a bad pin.
	branches := map[string]string{l.WriteUID: ""}
	for _, lr := range l.Reads {
		if b, ok := branches[lr.RepoUID]; !ok || b == "" {
			branches[lr.RepoUID] = lr.Branch
		}
	}
	// Resolve every member to its repo ID first, then reject any 12-hex prefix
	// collision (below) before validating branches.
	ids := make(map[string]string, len(branches)) // member uid → full repo ID
	ris := make(map[string]*RepoInstance, len(branches))
	for uid := range branches {
		ri := m.byUID[uid]
		if ri == nil {
			return fmt.Errorf("%w: %q", ErrRepoNotFound, uid)
		}
		id := ri.ID()
		if id == "" {
			return fmt.Errorf("repo %q has no resolvable ID", uid)
		}
		ids[uid] = id
		ris[uid] = ri
	}
	if err := checkMemberIDCollision(ids); err != nil {
		return err
	}
	for uid, branch := range branches {
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
		ris[uid].WithRead(func(svc *store.Service) {
			if svc == nil {
				lookupErr = fmt.Errorf("repo %q: store unavailable", uid)
				return
			}
			_, lookupErr = svc.Branches().HeadCommit(ctx, branch)
		})
		switch {
		case lookupErr == nil:
			// Branch resolves — pin is valid.
		case errors.Is(lookupErr, store.ErrBranchNotFound):
			return fmt.Errorf("%w: %q in repo %q", ErrLensBranchUnknown, branch, uid)
		default:
			return fmt.Errorf("validateLens: branch %q in repo %q: %w", branch, uid, lookupErr)
		}
	}
	return nil
}

// checkMemberIDCollision rejects a lens whose members collide on the 12-hex
// ROOT-COMMIT prefix Binding.ByID routes on (RFC §6.1): two members sharing
// that prefix would be misrouted, so dedup on the prefix rather than the full
// ID. This check stays root-commit based on purpose: membership is keyed by
// registry uid, but fact ADDRESSING is keyed by repo ID, and it is the
// addressing namespace that can collide.
//
// The ErrReplicaInLens name now covers only ONE reachable case. A true replica
// — two members with the SAME full repo ID — is unreachable by construction
// through the validated path: repos_active_repo_id makes a knowledge base
// unique among active repos, and a lens can only name active members. The
// sentinel is retained for the case that remains live: two DISTINCT knowledge
// bases whose root commits share a 12-hex prefix.
//
// ids maps member uid → full repo ID; keys are sorted so the error names the
// colliding pair deterministically.
func checkMemberIDCollision(ids map[string]string) error {
	uids := make([]string, 0, len(ids))
	for uid := range ids {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	seen := make(map[string]string, len(ids)) // 12-hex prefix → member uid
	for _, uid := range uids {
		id := ids[uid]
		prefix := id
		if len(id) >= 12 {
			prefix = id[:12]
		}
		if prev, dup := seen[prefix]; dup {
			return fmt.Errorf("%w: %q and %q share ID %s", ErrReplicaInLens, prev, uid, prefix)
		}
		seen[prefix] = uid
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
	if l.WriteUID == "" {
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
// Unlike CreateLens — and unlike RenameLens, which DOES reserve — this does not
// reserve the name in m.creating, and the reason is narrow: UpdateLens never
// CHANGES the name. Lens names became mutable on this branch, so "the name is
// immutable" is no longer why this is safe; what makes it safe is that the only
// name in play here is one the lens already durably holds. There is no new name
// being introduced into the shared repo/lens namespace, so there is nothing for
// P2's mutual exclusion to protect: a repo Create for that name still loses to
// the existing lens via its own lensNameConflict re-check, independent of this
// call. Any future edit that lets this method rewrite l.Name must add the
// reservation (see RenameLens for the shape and for what goes wrong without it).
//
// The write repo and description are pure input, checked up front. The name is
// re-validated (grammar) but never changed — the caller passes the existing name.
func (m *Manager) UpdateLens(ctx context.Context, l Lens) (Lens, error) {
	if !isValidRepoName(l.Name) {
		return Lens{}, fmt.Errorf("%w: %q", ErrInvalidLensName, l.Name)
	}
	if l.WriteUID == "" {
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

// LensRegistry returns the lens registry, or nil before Start.
func (m *Manager) LensRegistry() *LensRegistry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.registry
}

// Origins returns the per-repo origin store, or nil before Start.
func (m *Manager) Origins() *Origins {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.origins
}

// Repos returns the repo registry, or nil before Start.
func (m *Manager) Repos() *Registry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reg
}

// Get returns the RepoInstance for name, or nil if not found.
func (m *Manager) Get(name string) *RepoInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.repos[name]
}

// Set registers a RepoInstance under the given name, indexing it by uid too.
// If name already held a different instance (a swap or re-registration under
// the same name), its uid is evicted from byUID first — otherwise a
// previous-generation instance would keep byUID[oldUID] pointing at a dead
// instance forever.
func (m *Manager) Set(name string, ri *RepoInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.repos[name]
	if old != nil && (ri == nil || old.uid != ri.uid) {
		delete(m.byUID, old.uid)
	}
	m.repos[name] = ri
	if ri != nil && ri.uid != "" {
		m.byUID[ri.uid] = ri
	}
}

// GetByUID returns the RepoInstance with this registry uid, or nil.
func (m *Manager) GetByUID(uid string) *RepoInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byUID[uid]
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
	repoReg := m.reg
	m.reg = nil
	m.origins = nil
	m.mu.Unlock()
	if reg != nil {
		_ = reg.Close()
	}
	if repoReg != nil {
		// Origins shares repoReg's *sql.DB and has no Close of its own.
		_ = repoReg.Close()
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

// Start opens what the repo registry in cfg.Home/control.db says exists —
// NOT what happens to be sitting in cfg.Home/repos/ — and launches the
// background cluster-cache warmer. knomit has no default or otherwise
// privileged repo, and Start CREATES none. A fresh home therefore boots with
// zero repos registered, which is a valid steady state: repos arrive via
// Manager.Create (POST /api/v1/repos). A registered repo whose .db is
// missing, unopenable, or in conflict stays VISIBLE via Unavailable rather
// than vanishing. The warmer's behaviour comes from m.deps.Cfg.ClusterCache;
// check_interval=0 disables it. Callers must pair Start with a Close.
func (m *Manager) Start() error {
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	// Open the repo registry WITHOUT creating its schema, and store the handle
	// before anything can fail: the boot guard below reads "the repos table has
	// never existed here", and both creating that table and leaking the handle
	// on an early return would cost us something Close cannot recover.
	repoReg, err := OpenRegistryNoSchema(filepath.Join(m.deps.Cfg.Home, "control.db"))
	if err != nil {
		return fmt.Errorf("open repo registry: %w", err)
	}
	m.mu.Lock()
	m.reg = repoReg
	m.mu.Unlock()

	// Nothing above this line has written to control.db, so a refusal here
	// leaves the home exactly as migrate-registry expects to find it — and
	// leaves the evidence intact for the next boot attempt.
	if err := refuseUnmigratedHome(repoReg, reposDir); err != nil {
		return err
	}
	if err := repoReg.EnsureSchema(); err != nil {
		return err
	}

	reg, err := OpenLensRegistry(filepath.Join(m.deps.Cfg.Home, "control.db"))
	if err != nil {
		return fmt.Errorf("open control db: %w", err)
	}
	m.mu.Lock()
	m.registry = reg
	m.mu.Unlock()

	// One Crypt for the whole registry, from the same agent key each repo used
	// to derive its own. NewCrypt has no per-repo salt, so existing ciphertext
	// stays readable. A nil crypt disables credential STORAGE, not the server.
	var crypt *store.Crypt
	if keyData, kerr := os.ReadFile(m.deps.KeyPath); kerr != nil {
		log.Warn().Err(kerr).Str("key_path", m.deps.KeyPath).
			Msg("credential encryption unavailable: agent key unreadable; remote auth tokens cannot be stored")
	} else if c, cerr := store.NewCrypt(keyData); cerr != nil {
		log.Warn().Err(cerr).Msg("credential encryption unavailable: cannot derive key; remote auth tokens cannot be stored")
	} else {
		crypt = c
	}
	// OpenOrigins declares a foreign key into repos(uid), so it must follow
	// EnsureSchema.
	origins, err := OpenOrigins(repoReg.DB(), crypt)
	if err != nil {
		return fmt.Errorf("open repo origins: %w", err)
	}
	m.mu.Lock()
	m.origins = origins
	m.mu.Unlock()

	records, err := repoReg.List(StateActive)
	if err != nil {
		return fmt.Errorf("list registered repos: %w", err)
	}
	registered := make(map[string]struct{}, len(records))
	for _, rec := range records {
		registered[rec.UID] = struct{}{}
		m.openRegistered(rec)
	}
	// Archived repos are registered too — their database stays at
	// RepoPath(uid) and Restore reopens it in place. Counting only the active
	// ones would report every archived repo's file as an orphan, inviting an
	// operator to delete exactly the file a restore needs.
	archived, err := repoReg.List(StateArchived)
	if err != nil {
		return fmt.Errorf("list archived repos: %w", err)
	}
	for _, rec := range archived {
		registered[rec.UID] = struct{}{}
	}
	m.mu.Lock()
	m.orphanFiles = m.warnOrphanFiles(reposDir, registered)
	m.mu.Unlock()

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
// uid is its control.db identity and origin is the connection control.db holds
// for it (nil when it has none). Add opens what the registry says exists; it
// never writes to the registry itself.
//
// Add deliberately does NOT enforce ErrRepoNameConflictsLens (the reverse M-1
// guard). Add registers repos that already exist on disk — the Start
// discovery loop and the recovery paths inside Archive/Restore all go through
// here — so refusing a lens-name collision would DROP a repo whose collision
// predates this fix (or was created out-of-band), silently unregistering real
// data. The invariant is enforced loud at the user-facing creation boundary
// (CreatePreflight/Create/Restore) and soft at startup: an already-existing
// collision keeps its repo, and operators resolve it by renaming the lens.
func (m *Manager) Add(name, uid, dbPath string, origin *Origin) error {
	ri, err := m.openOne(name, uid, dbPath, origin)
	if err != nil {
		return err
	}
	m.Set(name, ri)
	return nil
}

// Remove unregisters a repo from the live maps without touching the registry
// or the filesystem. Callers own the durable state; this only detaches the
// runtime instance.
func (m *Manager) Remove(name string) {
	m.mu.Lock()
	ri := m.repos[name]
	delete(m.repos, name)
	if ri != nil {
		delete(m.byUID, ri.uid)
		// Same reason Archive drops it: an unregistered uid must not stay
		// flagged unavailable, or it resurfaces in GET /repos as a row nothing
		// backs. Inline rather than clearUnavailable — m.mu is already held.
		delete(m.unavailable, ri.uid)
	}
	m.mu.Unlock()
	if ri != nil {
		ri.shutdown()
	}
}

// ---------- private helpers ----------

// RepoPath is where a repo's database lives: <home>/repos/<uid>.db. The name
// is NOT part of the path — renaming a repo is a control.db UPDATE and never
// touches the filesystem.
func (m *Manager) RepoPath(uid string) string {
	return filepath.Join(m.deps.Cfg.Home, "repos", uid+".db")
}

// Unavailable describes a registered repo that has no live instance, with the
// reason it could not be opened. Reason is one of:
//
//   - "missing"    — the .db file is absent (offer rehydrate)
//   - "unopenable" — the file is there but the store or git failed to open
//   - "conflict"   — its knowledge base is already held by another active repo
//
// These are OBSERVED at open time and never stored, so they cannot drift out
// of sync with reality.
type Unavailable struct {
	Record RepoRecord
	Reason string
	Detail string
}

// Unavailable returns the registered repos with no live instance, sorted by
// name. They stay visible in the API — a repo that fails to open used to
// disappear entirely, with one ERROR line as its only trace.
func (m *Manager) Unavailable() []Unavailable {
	m.mu.RLock()
	out := make([]Unavailable, 0, len(m.unavailable))
	for _, u := range m.unavailable {
		out = append(out, u)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Record.Name < out[j].Record.Name })
	return out
}

// markUnavailable records why a registered repo has no live instance.
//
// The detail is usually an open error, which routinely embeds the server's
// absolute path to the database file — and it reaches users twice, as
// RepoSummary.detail in GET /repos and as the repo middleware's 409 body. Redact
// the home prefix here, at the single place both read from, and keep the
// unredacted text for the log line, which is where an operator debugs from.
func (m *Manager) markUnavailable(rec RepoRecord, reason string, detail string) {
	public := detail
	if home := m.deps.Cfg.Home; home != "" {
		public = strings.ReplaceAll(public, home, "<home>")
	}
	m.mu.Lock()
	m.unavailable[rec.UID] = Unavailable{Record: rec, Reason: reason, Detail: public}
	m.mu.Unlock()
	log.Warn().Str("repo", rec.Name).Str("uid", rec.UID).
		Str("reason", reason).Str("detail", detail).
		Msg("registered repo is unavailable; it stays listed in the API")
}

// clearUnavailable drops any unavailable record for uid — called when the repo
// comes back (rehydrate, or a successful open on a later boot).
func (m *Manager) clearUnavailable(uid string) {
	m.mu.Lock()
	delete(m.unavailable, uid)
	m.mu.Unlock()
}

// openRegistered opens one registry row, classifying every failure rather than
// dropping the repo.
func (m *Manager) openRegistered(rec RepoRecord) {
	reg, origins, herr := m.controlHandles()
	if herr != nil {
		m.markUnavailable(rec, "unopenable", herr.Error())
		return
	}
	dbPath := m.RepoPath(rec.UID)
	if _, err := os.Stat(dbPath); err != nil {
		m.markUnavailable(rec, "missing", "database file not found")
		return
	}
	origin, err := origins.Get(rec.UID)
	if err != nil {
		m.markUnavailable(rec, "unopenable", fmt.Sprintf("read origin: %v", err))
		return
	}
	ri, err := m.openOne(rec.Name, rec.UID, dbPath, origin)
	if err != nil {
		m.markUnavailable(rec, "unopenable", err.Error())
		return
	}
	// Record which knowledge base this repo holds. A conflict means another
	// ACTIVE repo already holds it — two local copies would both write
	// agent/<host> and clobber each other on push — so leave this one
	// unregistered and say so, rather than silently duplicating an identity.
	if id := ri.ID(); id != "" {
		if err := reg.RecordRepoID(rec.UID, id); err != nil {
			if errors.Is(err, ErrRepoAlreadyRegistered) {
				short := ri.ShortID()
				ri.shutdown()
				m.markUnavailable(rec, "conflict",
					fmt.Sprintf("knowledge base %s is already held by another active repo", short))
				return
			}
			log.Warn().Err(err).Str("repo", rec.Name).Msg("recording repo identity failed")
		}
	}
	m.clearUnavailable(rec.UID)
	m.Set(rec.Name, ri)
}

// warnOrphanFiles reports .db files under reposDir with no registry row. They
// are inert: dropping a database into the directory is no longer a way to
// register anything. One line each so a copied-in file is diagnosable.
//
// registered must carry ARCHIVED uids as well as active ones — an archived
// repo's database stays at RepoPath(uid) and Restore reopens it in place, so
// omitting them would report each one as an orphan and invite an operator to
// delete the file a restore needs.
//
// Returns the base names it warned about, so the classification is assertable;
// Start ignores the result.
func (m *Manager) warnOrphanFiles(reposDir string, registered map[string]struct{}) []string {
	dbFiles, _ := filepath.Glob(filepath.Join(reposDir, "*.db"))
	var orphans []string
	for _, p := range dbFiles {
		base := filepath.Base(p)
		if store.IsSessionDBFile(base) {
			continue
		}
		if _, ok := registered[strings.TrimSuffix(base, ".db")]; ok {
			continue
		}
		orphans = append(orphans, base)
		log.Warn().Str("file", base).
			Msg("database file is not in the registry and will be ignored")
	}
	return orphans
}

// refuseUnmigratedHome refuses to boot a home that predates the control.db repo
// registry, rather than coming up with every legacy .db invisible and a Create
// free to be told a taken name is available.
//
// THREE INDEPENDENT ARMS, each keyed on evidence that SURVIVES a failed boot:
// the legacy name-keyed lens column, a legacy archive directory holding
// archived repo databases, and the absence of the `repos` table itself. The
// first two are removed by `knomit migrate-registry` and by nothing else, so
// they clear exactly when the home is genuinely converted.
//
// Arm 3 is the one that catches a legacy home with no lenses and no archive —
// nothing but repos/<name>.db files — and it is the arm that has to be wired
// with care, TWICE over:
//
// It is NOT "the registry is empty". Purge deletes a repo's registry row before
// its file, so a failed unlink leaves an orphan .db on a fully migrated home
// whose table already existed. That home must still boot. Hence SchemaExisted,
// which distinguishes "never had a registry" from "has an empty one".
//
// And its evidence is destroyed by writing: whoever creates the `repos` table
// makes SchemaExisted report true forever after. That is why Manager.Start
// opens with OpenRegistryNoSchema and calls EnsureSchema only once this
// function has returned nil. Create the table on the way past and the guard
// fires exactly once — retry the boot and the server comes up on an unconverted
// home with every legacy .db invisible, which under a restart policy (systemd
// Restart=on-failure, Docker) turns "refuse loudly" into "refuse once, at 3am,
// into a log nobody reads".
func refuseUnmigratedHome(repoReg *Registry, reposDir string) error {
	const advice = "this home predates the control.db repo registry. Run `knomit migrate-registry` to convert it"

	legacyLenses, err := HasLegacyLensSchema(repoReg.DB())
	if err != nil {
		return fmt.Errorf("check control.db lens schema: %w", err)
	}
	if legacyLenses {
		return fmt.Errorf(
			"control.db still has the name-keyed lens tables (lenses.write_repo): %s", advice)
	}

	// The legacy archive lived at repos/archive/<ksuid>.db. anyRepoDBFile globs
	// one directory only, so without this arm a home whose repos are ALL
	// archived boots — and then every lens endpoint fails with a raw
	// "no such column: write_uid", because the legacy `lenses` table survives
	// CREATE TABLE IF NOT EXISTS untouched.
	archiveDir := filepath.Join(reposDir, "archive")
	if stray := anyRepoDBFile(archiveDir); stray != "" {
		return fmt.Errorf(
			"found %s in %s: %s", stray, archiveDir, advice)
	}

	if !repoReg.SchemaExisted() {
		if stray := anyRepoDBFile(reposDir); stray != "" {
			return fmt.Errorf(
				"found %s in %s but the repo registry is empty: %s", stray, reposDir, advice)
		}
	}
	return nil
}

// anyRepoDBFile returns the base name of the first non-session .db under dir,
// or "" if there is none. Deliberately not a filename-shape test — a repo
// name may legally look like a ksuid, so shape tells you nothing about
// whether a file is a stray repo database.
func anyRepoDBFile(dir string) string {
	dbFiles, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	sort.Strings(dbFiles)
	for _, p := range dbFiles {
		base := filepath.Base(p)
		if store.IsSessionDBFile(base) {
			continue
		}
		return base
	}
	return ""
}

// openOne initialises a single repo from a SQLite database file. It only ever
// OPENS: a database with no git data yields an error so the caller can skip it
// gracefully, never a freshly seeded repository.
func (m *Manager) openOne(name, uid, dbPath string, origin *Origin) (*RepoInstance, error) {
	b := repoBuilder{
		name:                  name,
		uid:                   uid,
		origin:                origin,
		dbPath:                dbPath,
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
	// ensureBranch must run before loadOntology: on a restored/copied home the
	// configured agent branch is absent until ensureBranch adopts it (issue
	// #32), and loadOntology reads (and may rewrite) the ontology file on
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
		ok := healIndexBranches(b.ctx, b.svc.IndexManager(), b.name, b.indexBranches, nil)
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
		ok := healIndexBranches(b.indexCtx, b.svc.IndexManager(), b.name, b.indexBranches, progress)
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

// OrphanFiles returns the .db base names the last Start found under repos/
// with no registry row, active or archived. Diagnostic only — these files are
// inert.
func (m *Manager) OrphanFiles() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.orphanFiles...)
}
