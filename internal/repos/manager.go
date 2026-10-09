package repos

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
	"github.com/ysmood/goob"
	"golang.org/x/crypto/ssh"

	"knomit/internal/client/sessions"
	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// Deps holds all shared resources needed to open and manage repos.
type Deps struct {
	Cfg         config.Config
	Signer      ssh.Signer
	AgentBranch string
	Embedder    store.BatchEmbedder // nil if unavailable
	// ScriptTools is the in-process MCP tool set a `do: script` trigger's
	// host calls (F07 PR 3). Injected — internal/mcp imports this package —
	// by internal/app as mcp.NewScriptTools(embedder). Nil means every host
	// call throws "no script tools"; the dispatcher still runs scripts.
	ScriptTools ScriptTools
	KeyPath     string
	// Machine configures every repo's lifecycle machine. Production leaves it
	// zero. Tests set Machine.Synchronous (no sync loop, no experiment sweep:
	// one inline sync round instead) and Machine.Hook (hold a stage or the
	// index job at a known point).
	Machine Options
	// CreateTimeout bounds a DETACHED create (Manager.StartCreate). Unset
	// means DefaultCreateTimeout. It is a real operational knob that tests
	// also set, to reach the timeout path in milliseconds rather than in
	// half-hours.
	CreateTimeout time.Duration
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

	// fleetBootWg tracks the one-shot fleet record reconcile Start launches
	// (fleetBootReconcile). Close waits for it before releasing control.db
	// and the repositories it reads and writes.
	fleetBootWg sync.WaitGroup

	// sessionCfg is the session block, parsed and validated once at the top
	// of Start. The reaper runs on it, and every instance opened afterwards
	// takes its resume window from it.
	sessionCfg sessionReaperConfig

	// serverAddr is this server's OWN address in KNOMIT_SERVER's spelling
	// (internal/serveraddr), set by whoever bound the listeners
	// (SetServerAddress). A recipe's `exec` child gets it as KNOMIT_SERVER, so
	// the `kb` it starts reaches THIS server. Nil until set.
	serverAddr atomic.Pointer[string]

	// repoEventHub fans every repo's REPO-LEVEL events into one server-wide
	// stream. Per-repo TaskHubs cannot serve the fleet-wide index chip: the web
	// app holds one events stream, for the ACTIVE repo, while rendering a chip
	// for every repo it lists. Created by New so it is available before Start.
	repoEventHub *RepoEventHub

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
	accepts *VerifyAccepts

	// clientSessions records every MCP client session (fourth control.db
	// tenant, borrowing reg's handle). Opened by Start, nil before; nil-safe
	// callers skip recording.
	clientSessions *sessions.Store

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

	// createJobs holds the detached create jobs (StartCreate), keyed by job
	// id, so a client that started a create and then lost its connection can
	// come back and ask how it ended. Finished entries are reaped past
	// CreateJobTTL on the registration path — see reapCreateJobsLocked.
	//
	// It has its OWN mutex rather than sharing m.mu or inflightMu: a status
	// poll must never queue behind a lifecycle operation holding m.mu, and a
	// job outlives the name reservation inflightMu guards.
	createJobsMu sync.Mutex
	createJobs   map[string]*CreateJob

	// createsCtx/createsCancel/createWg own the DETACHED CREATE lifecycle, the
	// third piece of background work Close must drain before it touches the
	// handles that work uses — after the index heal (indexWg) and the reconcile
	// loop (syncWg), and for the same reason: a create still running when
	// control.db closes issues SQL on a closed *sql.DB and keeps writing into a
	// directory the caller is now free to delete.
	//
	// SEPARATE from m.ctx, which Close cannot cancel because it belongs to
	// whoever called New. Close cancels createsCancel and then waits createWg,
	// so shutdown does not have to sit out a create's full CreateTimeout — and
	// the cancelled create unwinds through its own step-boundary checks and
	// cleanup() while control.db is STILL OPEN, which is what lets the rollback
	// actually complete instead of logging "registry row not removed".
	createsCtx    context.Context
	createsCancel context.CancelFunc
	createWg      sync.WaitGroup
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
	createsCtx, createsCancel := context.WithCancel(ctx)
	return &Manager{
		createsCtx:      createsCtx,
		createsCancel:   createsCancel,
		repos:           make(map[string]*RepoInstance),
		byUID:           make(map[string]*RepoInstance),
		unavailable:     make(map[string]Unavailable),
		ctx:             ctx,
		deps:            deps,
		creating:        make(map[string]struct{}),
		creatingOrigins: make(map[string]struct{}),
		// Created here rather than in Start: a repo can be mounted before
		// Start on some paths, and a nil hub would silently drop its events.
		repoEventHub: NewRepoEventHub(goob.New(ctx)),
	}
}

// RepoEvents subscribes to the server-wide repo-event stream until ctx ends.
// One stream for every repo — see RepoEventHub for why the per-repo TaskHub cannot
// serve the fleet-wide chip.
func (m *Manager) RepoEvents(ctx context.Context) goob.Events {
	return m.repoEventHub.Subscribe(ctx)
}

// ErrReplicaInLens rejects a lens mounting two replicas (same root-commit ID)
// of one repo: duplicated results, version confusion, ambiguous ID routing
// (RFC decision 18).
var ErrReplicaInLens = errors.New("lens mounts two replicas of the same repo")

// ErrLensWriteSubscribed is returned when a lens names a subscription as its
// write repo. A subscription has no agent branch and accepts no writes, so a
// lens writing through it could never commit anything.
var ErrLensWriteSubscribed = errors.New("lens write repo is a subscription (read-only)")

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
	// member. An explicit branch pin wins over the empty (read-branch) default
	// so a duplicate row can't hide a bad pin.
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
	// A subscription accepts no authored commits on any branch (Task 4's
	// read-only store), so a lens writing through it could never commit. Refused
	// here rather than at the first failed write, where the error would name a
	// branch instead of the real cause.
	if ris[l.WriteUID].Subscribed() {
		return fmt.Errorf("%w: %q", ErrLensWriteSubscribed, l.WriteUID)
	}
	if err := checkMemberIDCollision(ids); err != nil {
		return err
	}
	for uid, branch := range branches {
		if branch == "" {
			continue // read-branch default, always valid
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

// Signer returns this instance's commit signer, for a caller that opens a
// Service of its own and writes through it (the origin wizard's clone store).
// Every Service that commits must carry it: a signer-less write is
// store.ErrNoSigner. Set at construction and never changed, so no lock.
func (m *Manager) Signer() ssh.Signer { return m.deps.Signer }

// VerifyAccepts returns this instance's F09 accept list, or nil before Start.
func (m *Manager) VerifyAccepts() *VerifyAccepts {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accepts
}

// acceptListFor is the accept list bound to repository uid (nil before Start).
func (m *Manager) acceptListFor(uid string) store.AcceptList {
	return m.VerifyAccepts().For(uid)
}

// AcceptListFor is acceptListFor for a caller outside the package that opens
// a Service of its own (the origin wizard's clone store).
func (m *Manager) AcceptListFor(uid string) store.AcceptList { return m.acceptListFor(uid) }

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

// ControlDB returns the single control.db handle, or nil before Start. It is
// the same handle the repo registry owns and the client-session store
// borrows; stores that live in control.db without owning it take it from
// here rather than opening a second connection.
func (m *Manager) ControlDB() *sql.DB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.reg == nil {
		return nil
	}
	return m.reg.DB()
}

// SetServerAddress records this server's own address (serveraddr.ForLocal of
// the local listener it BOUND, else serveraddr.ForTCP of its TCP listener) for
// the processes it starts. Both server entry points call it once their
// listeners are bound: cmd/serve.go and the desktop's boot.
func (m *Manager) SetServerAddress(addr string) { m.serverAddr.Store(&addr) }

// ServerAddress is the address SetServerAddress recorded, or "" before it.
func (m *Manager) ServerAddress() string {
	if p := m.serverAddr.Load(); p != nil {
		return *p
	}
	return ""
}

// ClientSessions returns the client-session store, or nil before Start.
func (m *Manager) ClientSessions() *sessions.Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clientSessions
}

// SetClientSessions installs the store. Start calls it; tests call it to
// inject a store without a control.db home.
func (m *Manager) SetClientSessions(s *sessions.Store) {
	m.mu.Lock()
	m.clientSessions = s
	m.mu.Unlock()
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
// goroutines Start launched.
//
// Every machine is sent Unmount (which cancels its root context at once, so
// all of them wind down concurrently), then each is waited for, up to the
// unmount drain bound. Returns nil today; the error return matches io.Closer
// for forward compatibility.
func (m *Manager) Close() error {
	if m.sessionReaperStop != nil {
		m.sessionReaperStop()
		m.sessionReaperStop = nil
	}

	// Drain detached creates FIRST — before the control.db handles below are
	// nilled and closed: no in-flight background SQL may race the handle
	// closing, and a cancelled create rolls itself back, which deletes a
	// registry row and needs the registry still open to do it.
	if !m.drainCreates() {
		log.Error().Dur("timeout", createDrainTimeout).
			Msg("close: in-flight repo create did not finish after cancellation; closing anyway")
	}
	// The boot fleet reconcile reads control.db and writes one commit: it must
	// finish before those handles close, like the creates above.
	m.fleetBootWg.Wait()

	m.mu.Lock()
	reg := m.registry
	m.registry = nil
	repoReg := m.reg
	m.reg = nil
	m.origins = nil
	// Borrows repoReg's handle; dropping the pointer is the whole teardown.
	m.clientSessions = nil
	m.mu.Unlock()
	if reg != nil {
		// Non-owning: shares repoReg's handle, so this is a no-op.
		_ = reg.Close()
	}
	if repoReg != nil {
		// Owns the single control.db handle. Origins and the lens registry
		// both borrow it and have no Close of their own that does anything.
		_ = repoReg.Close()
	}

	m.mu.RLock()
	machines := make([]*Machine, 0, len(m.repos))
	for _, ri := range m.repos {
		if ri.machine != nil {
			machines = append(machines, ri.machine)
		}
	}
	m.mu.RUnlock()

	// Unmount cancels each machine's root context in Send, from this
	// goroutine, before the driver drains — so every repo winds down at once.
	var wg sync.WaitGroup
	for _, mc := range machines {
		wg.Add(1)
		go func(mc *Machine) {
			defer wg.Done()
			bound := mc.opts.unmountDrainBound()
			ctx, cancel := context.WithTimeout(context.Background(), bound)
			defer cancel()
			if _, err := mc.Send(ctx, Unmount("manager closing")); err != nil {
				log.Error().Err(err).Str("repo", mc.r.Name()).Dur("bound", bound).
					Msg("close: repo did not finish unmounting within the drain bound; closing anyway")
			}
		}(mc)
	}
	wg.Wait()
	return nil
}

// Send delivers a lifecycle event to ri's machine and waits for its reply (see
// Machine.Send). It is how callers outside the machine change a mounted repo;
// nothing else starts, stops or marks anything about it. A bare test instance
// has no machine and answers ErrNotOpen.
func (m *Manager) Send(ctx context.Context, ri *RepoInstance, e MachineEvent) (Reply, error) {
	if ri == nil || ri.machine == nil {
		return Reply{}, ErrNotOpen
	}
	return ri.machine.Send(ctx, e)
}

// unmount closes ri's machine and waits until it is done: its stores closed,
// its workers drained. Safe on a bare test instance.
func unmount(ri *RepoInstance, reason string) {
	if ri == nil || ri.machine == nil {
		return
	}
	_, _ = ri.machine.Send(context.Background(), Unmount(reason))
}

// newInstance builds a repo's runtime instance and starts its lifecycle
// machine, unmounted. Everything here is fixed for the instance's whole life:
// the agent branch and the subscription flag move together (a subscription has
// no agent branch, kb/invariants/repos/subscription/flag-and-branch-paired),
// and the trigger dispatcher, consensus merger and experiment sweeper objects
// exist from the first commit so their kick slots do too. The stages fill in
// the rest when the machine is mounted.
func (m *Manager) newInstance(name, uid string, subscribed bool) *RepoInstance {
	cfg := m.deps.Cfg
	agentBranch := m.deps.AgentBranch
	if subscribed {
		agentBranch = ""
	}
	ri := &RepoInstance{
		uid:                           uid,
		dbPath:                        m.RepoPath(uid),
		agentBranch:                   agentBranch,
		subscribed:                    subscribed,
		embedder:                      m.deps.Embedder,
		ontologyRoot:                  cfg.OntologyRoot,
		methodologyMinScore:           cfg.MethodologyMinScore,
		clusterResolution:             clusterResolutionOrDefault(cfg.ClusterCache.Resolution),
		clusterMinCommunity:           clusterMinCommunityOrDefault(cfg.ClusterCache.MinCommunitySize),
		clusterNeighborKinds:          clusterNeighborKindsOrDefault(cfg.ClusterCache.NeighborKinds),
		discoveryEffortDefault:        cfg.Discovery.EffortDefault,
		pipelineResumeWindow:          m.sessionCfg.PipelineResumeWindow,
		discoveryConfidenceThreshold:  cfg.Discovery.ConfidenceThreshold,
		discoveryBlastRadiusThreshold: cfg.Discovery.BlastRadiusThreshold,
		discoveryBridge:               cfg.Discovery.Bridge,
		discoveryCohFloor:             cfg.Discovery.CohFloor,
		discoveryMaxMembers:           cfg.Discovery.MaxMembers,
		discoveryQualityFloor:         cfg.Discovery.QualityFloor,
		discoveryWCoh:                 cfg.Discovery.WCoh,
		discoveryWGap:                 cfg.Discovery.WGap,
		discoveryWSpec:                cfg.Discovery.WSpec,
		hub:                           NewTaskHub(m.ctx),
		syncWake:                      make(chan struct{}, 1),
		breakers:                      &syncBreakers{},
		repoEventHub:                  m.repoEventHub,
		env: stageEnv{
			m:        m,
			cfg:      cfg,
			signer:   m.deps.Signer,
			keyPath:  m.deps.KeyPath,
			embedder: m.deps.Embedder,
			onPush:   m.fleetPushed,
		},
	}
	ri.setName(name)
	ri.ident.Store(&identity{ontologyErr: errNotIdentified})
	// F07 / F08: built for a writable repo with an agent branch on a
	// writable server; nil otherwise, and the kicks below are nil checks.
	if agentBranch != "" && !cfg.ReadOnly {
		ri.triggers = newTriggerDispatcher(ri, name, agentBranch, m.deps.Signer, cfg.Log.SlowTriggerMS,
			cfg.Triggers.ScriptRatePerMinute, m.deps.ScriptTools, cfg.Home, m.ServerAddress)
		ri.consensus = newConsensusMerger(ri, name, agentBranch)
	}
	// No sweeper for a subscription (no experiment can be forked from one)
	// or when experiments.expiry_days is 0 (never expire: the loop must not
	// run at all).
	if !subscribed && cfg.Experiments.ExpiryDays > 0 {
		ri.sweep = newExperimentSweeper(ri.Acquire, name, cfg.Experiments.ExpiryDays)
	}
	// The store's commit callback runs under the writer's branch lock, so it
	// only schedules: the observer's debounce, and one non-blocking send on a
	// 1-slot channel — the dispatcher's for the agent branch (plus the sync
	// wake under `sync: {push: realtime}`), the consensus merger's for every
	// other branch except exp/*. It never reads the store.
	ri.onCommit = func(branch, hash string) {
		if obs := ri.observer.Load(); obs != nil {
			obs.Notify(hash)
		}
		if branch == agentBranch {
			ri.triggerKick()
			if ri.realtimePush.Load() {
				ri.wakeSync()
			}
		} else if consensusKicks(branch, agentBranch) {
			ri.consensusKick()
		}
	}
	newMachine(m.ctx, ri, m.deps.Machine)
	return ri
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
	// The session block is validated before anything opens: an invalid one
	// is a boot error, and every instance takes its resume window from it.
	sessionCfg, err := parseSessionReaperConfig(m.deps.Cfg.Session)
	if err != nil {
		return fmt.Errorf("session config: %w", err)
	}
	m.sessionCfg = sessionCfg

	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	// Open the repo registry WITHOUT creating its schema, and store the handle
	// before anything can fail, so a controlUp error below cannot leak it:
	// Close releases whatever m.reg holds.
	repoReg, err := OpenRegistryNoSchema(filepath.Join(m.deps.Cfg.Home, "control.db"))
	if err != nil {
		return fmt.Errorf("open repo registry: %w", err)
	}
	m.mu.Lock()
	m.reg = repoReg
	m.mu.Unlock()

	// controlUp holds the load-bearing ordering — the lens re-key before the
	// versioned baseline — and is shared with OpenRegistry and OpenLensRegistry
	// so no entry point can migrate this file without it.
	if err := controlUp(repoReg.DB()); err != nil {
		return err
	}
	// The instance identity row (F09): app.New writes it before the Manager
	// exists; this only covers a Manager booted without app (tests, tools), so
	// the fleet state machine always has its row.
	if m.deps.AgentBranch != "" {
		if _, err := resolveIdentity(repoReg.DB(), m.deps.AgentBranch, time.Now()); err != nil {
			return err
		}
	}

	// One handle for all three tenants: Registry owns it, the lens registry and
	// Origins borrow it. Sharing is what lets the lens foreign keys into
	// repos(uid) be enforced on the same connection.
	reg := NewLensRegistry(repoReg.DB())
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
	// repo_origins declares a foreign key into repos(uid), so this must follow
	// the migration that creates both — OpenOrigins itself cannot check, and
	// cannot fail.
	origins := OpenOrigins(repoReg.DB(), crypt)
	m.mu.Lock()
	m.origins = origins
	// F09's accept list, another control.db tenant (control migration 000013).
	m.accepts = OpenVerifyAccepts(repoReg.DB())
	m.mu.Unlock()

	// Client-session registry: fourth tenant of control.db, borrowing the same
	// handle. A malformed [session] client_* block surfaces at boot, like the
	// reaper's.
	policy, err := sessions.ParsePolicy(m.deps.Cfg.Session.ClientDeadAfter,
		m.deps.Cfg.Session.ClientHiddenAfter, m.deps.Cfg.Session.ClientRetention)
	if err != nil {
		return fmt.Errorf("client session policy: %w", err)
	}
	if policy.Retention == 0 {
		log.Warn().Msg("client sessions: retention is 0 — rows are never purged; control.db grows one row per client session")
	}
	// WithHub on m.ctx: the change hub broadcasts row changes to browsers
	// subscribed to GET /api/v1/sessions/events, and its lifetime is the
	// server's. A Store built without it still writes — that is what every
	// test in the sessions package relies on.
	m.SetClientSessions(sessions.New(repoReg.DB(), policy).WithHub(m.ctx))

	records, err := repoReg.List(StateActive)
	if err != nil {
		return fmt.Errorf("list registered repos: %w", err)
	}
	// Every active row is mounted concurrently: a mount replies once its walk
	// reaches Ready, which is milliseconds — the index job runs in the
	// background — so boot does not wait on any repo's index.
	registered := make(map[string]struct{}, len(records))
	var boot sync.WaitGroup
	for _, rec := range records {
		registered[rec.UID] = struct{}{}
		boot.Add(1)
		go func(rec RepoRecord) {
			defer boot.Done()
			m.openRegistered(rec)
		}(rec)
	}
	boot.Wait()
	m.autoArchiveRefused()
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

	// Launch the background idle-session reaper on the session block parsed
	// at the top of Start.
	m.sessionReaperStop = m.startSessionReaper(m.sessionCfg)

	// F10: bring this instance's fleet member record up to date with the
	// current config and binary, off the startup path (it never blocks or
	// fails Start).
	m.startFleetBootReconcile()
	return nil
}

// mountExisting builds the instance for a registered repo and mounts what is
// on disk: Populate does nothing, Open → Identify → Index → Serve → Sync. It
// replies once the walk reaches Ready (the index job keeps running). A failed
// mount has already unwound itself; the machine is unmounted and the error
// (a *StageError naming the stage) returned.
//
// It registers nothing. It deliberately does not enforce
// ErrRepoNameConflictsLens either: it opens repos that already exist, and
// refusing a lens-name collision here would DROP a repo whose collision
// predates that guard. The invariant is enforced loud at the user-facing
// creation boundary (CreatePreflight/Create/Restore) and soft here.
func (m *Manager) mountExisting(name, uid string, origin *Origin) (*RepoInstance, error) {
	subscribed := origin != nil && origin.Mode == OriginModeSubscribe
	ri := m.newInstance(name, uid, subscribed)
	if _, err := m.Send(context.Background(), ri, Mount(MountSpec{Mode: MountExisting})); err != nil {
		unmount(ri, "mount failed")
		return nil, err
	}
	return ri, nil
}

// Remove unregisters a repo from the live maps without touching the registry
// or the filesystem, and unmounts it. Callers own the durable state.
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
	unmount(ri, "removed")
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
	// Cause is the typed refusal behind Reason, when there is one the code acts
	// on: CauseOntologySymlink makes Start archive the repo (autoArchiveRefused).
	// Empty for every other failure. Keyed on by code — never match Detail text.
	Cause UnavailableCause
}

// UnavailableCause names a refusal precisely enough to act on.
type UnavailableCause string

const (
	// CauseOntologySymlink: the ontology on the read branch is a symlink, which
	// Identify refuses (fact.ErrSymlinkNotFollowed). Reason "unopenable".
	CauseOntologySymlink UnavailableCause = "ontology_symlink"
	// CauseIdentityConflict: another active repo holds this knowledge base
	// (ErrRepoAlreadyRegistered). Reason "conflict".
	CauseIdentityConflict UnavailableCause = "identity_conflict"
)

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
	m.markUnavailableCause(rec, reason, "", detail)
}

// markUnavailableCause is markUnavailable with a typed cause.
func (m *Manager) markUnavailableCause(rec RepoRecord, reason string, cause UnavailableCause, detail string) {
	public := m.redactHome(detail)
	m.mu.Lock()
	m.unavailable[rec.UID] = Unavailable{Record: rec, Reason: reason, Detail: public, Cause: cause}
	m.mu.Unlock()
	log.Warn().Str("repo", rec.Name).Str("uid", rec.UID).
		Str("reason", reason).Str("detail", detail).
		Msg("registered repo is unavailable; it stays listed in the API")
}

// autoArchiveRefused archives, at boot, every repo the mount refused for a
// reason that is (1) a property of the repo's own content, (2) unable to heal
// in place, and (3) independent of which repo opened first. Today that is ONE
// cause: a symlinked ontology (CauseOntologySymlink). A refused repo does not
// sync, so no fix can reach it; archived, its name and knowledge base stop
// blocking a re-add, and it can be purged. Nothing is deleted: the database
// stays at RepoPath(uid), and a restore re-runs Identify, refusing with the
// same text until the store changes.
//
// What it deliberately leaves UNAVAILABLE (the user archives those):
//   - missing / other unopenable: they can heal by themselves (file put back,
//     binary upgraded) and archiving would turn that into a manual restore;
//   - conflict: choosing which of two copies of one knowledge base to keep is
//     the user's call, and the loser here is decided by mount order;
//   - a repo a lens reads or writes: archive refuses it (ErrRepoInUseByLens),
//     so it stays unavailable with a WARN naming the lenses.
//
// Runs once, in Start after every mount replied — never at runtime, where an
// archive would pull a repo out from under live sessions.
func (m *Manager) autoArchiveRefused() {
	for _, u := range m.Unavailable() {
		if u.Cause != CauseOntologySymlink {
			continue
		}
		_, err := m.archive(u.Record.Name, ArchiveReason{
			Source: ArchiveBySystem,
			Reason: u.Detail,
			// The state alone: the detail IS the reason, said once.
			Condition: u.Reason,
		})
		if err != nil {
			log.Warn().Err(err).Str("repo", u.Record.Name).Str("uid", u.Record.UID).
				Msg("refused repo not archived automatically; it stays listed as unavailable")
			continue
		}
		log.Warn().Str("repo", u.Record.Name).Str("uid", u.Record.UID).Str("reason", u.Detail).
			Msg("refused repo archived automatically; restore, or purge and add it again once its ontology is a file")
	}
}

// redactHome replaces the server's home directory in text bound for a user
// (an API body, a stored archive reason) with "<home>".
func (m *Manager) redactHome(text string) string {
	if home := m.deps.Cfg.Home; home != "" {
		return strings.ReplaceAll(text, home, "<home>")
	}
	return text
}

// clearUnavailable drops any unavailable record for uid — called when the repo
// comes back (rehydrate, or a successful open on a later boot).
func (m *Manager) clearUnavailable(uid string) {
	m.mu.Lock()
	delete(m.unavailable, uid)
	m.mu.Unlock()
}

// openRegistered mounts one registry row, classifying every failure rather
// than dropping the repo: a missing file, a store that will not open, and a
// knowledge base already held by another active repo (two local copies would
// both write agent/<host> and clobber each other on push) each leave the repo
// VISIBLE as unavailable, with the reason.
func (m *Manager) openRegistered(rec RepoRecord) {
	_, origins, herr := m.controlHandles()
	if herr != nil {
		m.markUnavailable(rec, "unopenable", herr.Error())
		return
	}
	if _, err := os.Stat(m.RepoPath(rec.UID)); err != nil {
		m.markUnavailable(rec, "missing", "database file not found")
		return
	}
	origin, err := origins.Get(rec.UID)
	if err != nil {
		m.markUnavailable(rec, "unopenable", fmt.Sprintf("read origin: %v", err))
		return
	}
	ri, err := m.mountExisting(rec.Name, rec.UID, origin)
	if err != nil {
		reason, cause, detail := unavailableReason(err)
		m.markUnavailableCause(rec, reason, cause, detail)
		return
	}
	m.clearUnavailable(rec.UID)
	m.Set(rec.Name, ri)
}

// unavailableReason maps a failed mount onto the Unavailable vocabulary: an
// identity conflict is "conflict", every other failure "unopenable". The cause
// is set for the refusals code acts on (see UnavailableCause).
func unavailableReason(err error) (reason string, cause UnavailableCause, detail string) {
	var se *StageError
	if errors.As(err, &se) {
		if errors.Is(se.Err, ErrRepoAlreadyRegistered) {
			return "conflict", CauseIdentityConflict, se.Err.Error()
		}
		if errors.Is(se.Err, fact.ErrSymlinkNotFollowed) {
			return "unopenable", CauseOntologySymlink, se.Err.Error()
		}
		return "unopenable", "", se.Err.Error()
	}
	return "unopenable", "", err.Error()
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
