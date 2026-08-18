package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/segmentio/ksuid"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

var (
	// ErrRepoExists is returned when a Create targets a name that is already active.
	ErrRepoExists = errors.New("repo already exists")
	// ErrInvalidName is returned when a repo name fails validation.
	ErrInvalidName = errors.New("invalid repo name")
	// ErrCreateInFlight is returned when a Create or Restore is already bringing
	// this name into the active map. It gates every operation that registers a
	// name, so concurrent Create/Restore on the same name can't race.
	ErrCreateInFlight = errors.New("an operation is already in flight for this name")
	// ErrOriginInUse is returned when a clone/restore would point a second active
	// repo at an origin URL already used by an active repo.
	ErrOriginInUse = errors.New("origin URL already in use by an active repo")
	// ErrArchiveNotFound is returned when no archived repo matches.
	ErrArchiveNotFound = errors.New("archived repo not found")
	// ErrRepoNotFound is returned when no active repo matches a name.
	ErrRepoNotFound = errors.New("repo not found")
	// ErrRepoNameConflictsLens rejects creating or restoring a repo whose name
	// equals an existing lens name. It is the reverse-direction twin of
	// ErrLensNameConflictsRepo. The cursor-pinning identity (RFC §7.3) is now
	// Binding.PinID() — repo:<uid> / lens:<uid> — which cannot collide between
	// a lens and a repo even if they share a name, so this guard is no longer
	// what keeps a same-name cursor cross-resume closed; it survives as
	// namespace legibility (one name must never serve two endpoints).
	// ValidateLens closes the direction where the lens is created second; this
	// closes the direction where the repo is created (or restored) second, so
	// gotcha M-1 (kb/gotchas/lens/cursor-binding-pin) is enforced
	// bidirectionally rather than one-way. It is checked in the user-facing
	// name-introducing paths (CreatePreflight, Create, Restore) but deliberately
	// NOT in Manager.Add — see the comment on Add.
	ErrRepoNameConflictsLens = errors.New("repo name conflicts with an existing lens name")
	// ErrManagerStopped is returned when a lifecycle operation arrives before
	// Start has opened the control.db tenants or after Close has released them.
	// In practice it is the shutdown race: an in-flight POST /api/v1/repos that
	// reached Create while Close was tearing the manager down.
	ErrManagerStopped = errors.New("repo manager is not running")
)

// controlHandles snapshots the two control.db tenants under m.mu.
//
// Every lifecycle operation goes through this rather than reading m.reg /
// m.origins as bare fields. Start assigns them under the write lock and Close
// nils them under the same lock, so a bare read from a request goroutine is an
// unsynchronised read of a field another goroutine writes — and, once Close has
// run, a nil dereference in the middle of an operation that has already begun.
//
// It is the counterpart of the Repos()/Origins() accessors, taken ONCE per
// operation: an operation that re-read the fields between steps could see them
// go nil halfway through, which is a worse failure than starting late and
// finishing against handles that are closing.
//
// Callers must not already hold m.mu — sync.RWMutex is not reentrant. Archive
// takes its snapshot before locking for exactly that reason.
func (m *Manager) controlHandles() (*Registry, *Origins, error) {
	m.mu.RLock()
	reg, origins := m.reg, m.origins
	m.mu.RUnlock()
	if reg == nil || origins == nil {
		return nil, nil, ErrManagerStopped
	}
	return reg, origins, nil
}

// lensNameConflict reports ErrRepoNameConflictsLens when name already exists as
// a lens in the registry, enforcing the reverse direction of the lens/repo
// name-disjointness invariant (gotcha M-1). It returns nil when the registry is
// not yet open (before Start), matching how Archive skips its lens check on a
// nil registry: fail soft at startup, fail loud at creation.
//
// Callers are the user-facing paths that introduce a repo name — CreatePreflight,
// Create, Restore and RenameRepo — every one of which does NOT hold m.mu at its
// call site, so the LensRegistry() accessor (which takes m.mu.RLock) is safe
// here; no self-deadlock as in Archive's direct-field read. Keep it that way:
// calling this from under m.mu deadlocks.
//
// RenameLens enforces the SAME disjointness invariant but is deliberately not a
// caller. It runs entirely under m.mu (so this function would self-deadlock),
// and it needs the opposite direction anyway — "does a REPO hold this name",
// which it reads straight off m.repos. Between the two, both directions of
// gotcha M-1 are covered.
func (m *Manager) lensNameConflict(name string) error {
	reg := m.LensRegistry()
	if reg == nil {
		return nil
	}
	_, ok, err := reg.Get(name)
	if err != nil {
		return fmt.Errorf("lens registry: %w", err)
	}
	if ok {
		return fmt.Errorf("%w: %q", ErrRepoNameConflictsLens, name)
	}
	return nil
}

// OriginSpec describes a git remote to attach at create/restore time.
type OriginSpec struct {
	URL        string
	Branch     string
	AuthMethod string
	AuthToken  string
}

// CreateSpec is the single input for all repo-creation modes.
//
// The two remote modes answer ONE question about the chosen branch — does it
// already carry a knomit ontology? — and are the two halves of its answer:
// "clone" joins a branch that has one, "initialize" writes one onto knomit's
// own agent branch cut from a branch that has not. Neither ever creates or
// writes a branch on the remote other than agent/<host>.
type CreateSpec struct {
	Name           string
	Mode           string // "preset" | "custom" | "clone" | "initialize"
	OntologyPreset string
	OntologyYAML   string
	Origin         *OriginSpec
}

// hasRemote reports whether spec's mode attaches a remote origin. "clone" and
// "initialize" are the two remote-bearing modes — everywhere one of them needs
// the origin checked, reserved, persisted, or synced, the other needs it too.
// Written once here rather than repeated as `|| spec.Mode == "initialize"` at
// each of the five sites in this file, so a future sixth site can't be added
// without the same check.
func (s CreateSpec) hasRemote() bool {
	return s.Mode == "clone" || s.Mode == "initialize"
}

// Event is a progress message emitted during Create.
type Event struct {
	Step    string `json:"step"`
	Message string `json:"message"`
	Pct     int    `json:"pct"`
}

// CreatePreflight runs the checks that must surface as an HTTP status BEFORE
// any streaming begins: name validity, clone-origin presence/uniqueness, name
// already active, create-in-flight, and — for mode "initialize" only — that
// the remote has at least one branch. The authoritative guards still live
// inside Create.
//
// The initialize probe is the one network call here, and it is deliberate:
// without it ErrRemoteNoBranches could only ever reach a caller as a
// {"type":"error"} line inside an already-committed 200 stream, so the 409 the
// API documents for that case would be unreachable. It is bounded by
// Cfg.Git.NetworkTimeout (ProbeOrigin) and takes ctx, so a caller that goes
// away stops it.
//
// Only the definitive "the remote has NO refs" verdict fails here. An
// unreachable remote, an auth-required one, or a probe the origin gate
// refused all fall through to Create, which reports them through the stream
// exactly as before — a pre-stream failure is worth having only where the
// answer is certain, and a probe that could not see the remote has not
// established anything. Create re-asserts this regardless (initInitialize runs
// its own probe): this one is advisory, and a remote can gain or lose refs
// between the two.
func (m *Manager) CreatePreflight(ctx context.Context, spec CreateSpec) error {
	if !isValidRepoName(spec.Name) {
		return ErrInvalidName
	}
	origin := ""
	if spec.hasRemote() {
		if spec.Origin == nil || spec.Origin.URL == "" {
			return fmt.Errorf("%w: %s mode requires origin.url", ErrInvalidName, spec.Mode)
		}
		origin = spec.Origin.URL
		if spec.Mode == "clone" {
			if err := rejectOntologySpecForClone(spec); err != nil {
				return err
			}
		} else if spec.OntologyPreset == "" && spec.OntologyYAML == "" {
			// Initializing without an ontology has no meaning: writing
			// .knomit/ontology.yaml IS the act that turns the branch into a
			// knowledge base, and there would be nothing to write.
			return fmt.Errorf("%w: initialize mode requires ontology_preset or ontology_yaml", ErrInvalidName)
		}
		if active := m.ActiveRepoWithOrigin(origin); active != "" {
			return fmt.Errorf("%w: %q", ErrOriginInUse, active)
		}
	}
	if m.Get(spec.Name) != nil {
		return ErrRepoExists
	}
	if err := m.lensNameConflict(spec.Name); err != nil {
		return err
	}
	m.inflightMu.Lock()
	_, nameInflight := m.creating[spec.Name]
	_, originInflight := m.creatingOrigins[origin]
	m.inflightMu.Unlock()
	if nameInflight {
		return ErrCreateInFlight
	}
	if origin != "" && originInflight {
		return fmt.Errorf("%w (clone in flight)", ErrOriginInUse)
	}
	// Last, because it is the only check here that touches the network: every
	// cheap local refusal above should cost nothing.
	//
	// The refusal is the INVERSE of the one seed mode used to make here. Seed
	// needed a ref-less remote and refused one with refs; initialize needs a
	// branch to cut its agent branch from and refuses one WITHOUT refs. That
	// inversion is the whole point of the redesign: knomit never creates a
	// branch on the remote other than its own, so it can no longer be the thing
	// that pushes a protected default branch into existence.
	if spec.hasRemote() {
		// A ref-less remote is refused for BOTH remote modes. knomit never
		// creates a branch on a remote other than its own agent branch, so
		// there is nothing to cut that branch from and nothing to clone —
		// initialize needs a tip to start from, and clone needs an ontology
		// that cannot exist where no branch does.
		// Refs only. This asks whether the remote has branches; whether we may
		// PUSH is the wizard's question, and answering it here cost a second
		// handshake — the one that dials without a usable context bound — on
		// every create, while also exposing the create to a "denied" verdict it
		// does not act on.
		if probe, perr := m.ProbeOriginRefs(ctx, *spec.Origin); perr == nil &&
			probe.Reachable && !probe.AuthRequired && probe.Empty {
			return ErrRemoteNoBranches
		}
		// And the SHAPE question: is the branch this create will read already a
		// knowledge base? Each mode has exactly one answer it can work with.
		//
		// Established here so the refusal is a 409 with the stream still shut.
		// Both conditions were reachable only from inside Create — i.e. after
		// w.WriteHeader(200) — which made a documented 409 impossible to
		// receive, and left a client unable to tell "your remote is the wrong
		// shape for this mode" from "the create broke halfway through". The
		// authoritative checks stay where they are: this is a fail-fast
		// affordance run against an earlier moment in time, exactly as the
		// no-branches probe above is.
		//
		// The UNKNOWN answer refuses nothing. A check that did not complete
		// established nothing, and turning that into a refusal would block a
		// create that is very likely fine; Create's own check will decide with
		// the connection it actually opens.
		if init, ierr := m.ProbeInitialized(ctx, *spec.Origin); ierr == nil {
			switch {
			case spec.Mode == "clone" && init.Initialized == InitializedNo:
				return ErrRemoteNotInitialized
			case spec.Mode == "initialize" && init.Initialized == InitializedYes:
				return ErrRemoteAlreadyInitialized
			}
		}
	}
	return nil
}

// reserveNameAndOrigin reserves name and (when non-empty) origin for the
// duration of an operation that brings a name into the active map. It is the
// single mutual-exclusion gate every Add path must hold across its Get-check →
// Add window, so two concurrent Create/Restore calls can't both register the
// same name or attach the same origin to two repos.
//
// On success the returned release func frees both reservations; it must be
// deferred so it runs strictly after Add — that overlap (reserved while also
// registered) is what makes the active-origin scan below gap-free.
//
// Errors: ErrCreateInFlight if name is already reserved; ErrOriginInUse if
// origin is reserved by another in-flight op or already attached to an active
// repo. A pass with origin=="" (preset/custom create) reserves the name only.
func (m *Manager) reserveNameAndOrigin(name, origin string) (func(), error) {
	m.inflightMu.Lock()
	if _, ok := m.creating[name]; ok {
		m.inflightMu.Unlock()
		return nil, ErrCreateInFlight
	}
	if origin != "" {
		if _, ok := m.creatingOrigins[origin]; ok {
			m.inflightMu.Unlock()
			return nil, fmt.Errorf("%w (clone in flight)", ErrOriginInUse)
		}
	}
	m.creating[name] = struct{}{}
	if origin != "" {
		m.creatingOrigins[origin] = struct{}{}
	}
	m.inflightMu.Unlock()

	release := func() {
		m.inflightMu.Lock()
		delete(m.creating, name)
		if origin != "" {
			delete(m.creatingOrigins, origin)
		}
		m.inflightMu.Unlock()
	}

	// Scan active repos only after reserving: any concurrent clone/restore of
	// this origin is now blocked above, so an active match here is the
	// authoritative origin-uniqueness verdict. Done outside inflightMu so we
	// don't couple it to mu (ActiveRepoWithOrigin takes mu + per-repo locks).
	if origin != "" {
		if active := m.ActiveRepoWithOrigin(origin); active != "" {
			release()
			return nil, fmt.Errorf("%w: %q", ErrOriginInUse, active)
		}
	}
	return release, nil
}

// Create initialises a new repo on disk per spec, registers it, and (for clone
// mode) attaches the origin and activates sync. Progress is reported via emit.
//
// Cancellation is honoured at step boundaries: ctx is checked before each
// init step and again before the repo is registered, and a cancelled Create
// removes the partial .db before returning ctx.Err(). The network fetch inside
// clone mode is not itself interruptible (the store's clone is not yet
// context-aware), so an in-flight clone runs to completion before the next
// boundary check fires.
func (m *Manager) Create(ctx context.Context, spec CreateSpec, emit func(Event)) (*RepoInstance, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	if !isValidRepoName(spec.Name) {
		return nil, ErrInvalidName
	}
	reg, origins, err := m.controlHandles()
	if err != nil {
		return nil, err
	}

	// Determine the origin to reserve (clone/seed only) before reserving, so the
	// reservation covers the whole clone/seed — including the network fetch —
	// and a second clone/seed of the same origin is blocked for that entire
	// window.
	var origin string
	if spec.hasRemote() {
		if spec.Origin == nil || spec.Origin.URL == "" {
			return nil, fmt.Errorf("%w: %s mode requires origin.url", ErrInvalidName, spec.Mode)
		}
		origin = spec.Origin.URL
	}

	if m.Get(spec.Name) != nil {
		return nil, ErrRepoExists
	}
	if err := m.lensNameConflict(spec.Name); err != nil {
		return nil, err
	}
	release, err := m.reserveNameAndOrigin(spec.Name, origin)
	if err != nil {
		return nil, err
	}
	defer release()
	if m.Get(spec.Name) != nil { // re-check after reserving
		return nil, ErrRepoExists
	}
	if err := m.lensNameConflict(spec.Name); err != nil { // re-check after reserving
		return nil, err
	}

	emit(Event{Step: "validate", Message: "validated request", Pct: 5})

	// Mint the identity and claim the name in one INSERT. The uid is new, so
	// the file path below is fresh BY CONSTRUCTION — the old "leftover .db file"
	// guard is unreachable now that a name can no longer collide with a file.
	uid := ksuid.New().String()
	rec := RepoRecord{
		UID:       uid,
		Name:      spec.Name,
		State:     StateActive,
		Profile:   ProfileCode,
		CreatedAt: time.Now().UTC().Unix(),
	}
	if ierr := reg.Insert(rec); ierr != nil {
		return nil, ierr // ErrRepoExists when an active repo already holds the name
	}
	dbPath := m.RepoPath(uid)
	cleanup := func() {
		os.Remove(dbPath)
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
		if derr := reg.Delete(uid); derr != nil {
			log.Error().Err(derr).Str("repo", spec.Name).Str("uid", uid).
				Msg("create rollback: registry row not removed; it will report as missing")
		}
	}

	if cerr := ctx.Err(); cerr != nil {
		cleanup()
		return nil, cerr
	}

	var resolvedUpstream string
	switch spec.Mode {
	case "preset", "custom":
		if ierr := m.initLocal(ctx, spec, dbPath, emit); ierr != nil {
			cleanup()
			return nil, ierr
		}
	case "clone":
		// Name/origin presence and uniqueness were validated and reserved up
		// front via reserveNameAndOrigin; just clone.
		upstream, ierr := m.initClone(ctx, spec, dbPath, emit)
		if ierr != nil {
			cleanup()
			return nil, ierr
		}
		resolvedUpstream = upstream
	case "initialize":
		// Name/origin presence and uniqueness were validated and reserved up
		// front via reserveNameAndOrigin; just initialize.
		upstream, ierr := m.initInitialize(ctx, spec, dbPath, emit)
		if ierr != nil {
			cleanup()
			return nil, ierr
		}
		resolvedUpstream = upstream
	default:
		cleanup()
		return nil, fmt.Errorf("%w: unknown mode %q", ErrInvalidName, spec.Mode)
	}

	// An initialize that got this far has already PUSHED its agent branch.
	// Every failure from here on calls cleanup() — deleting the local .db and
	// the registry row — while the remote KEEPS that branch. It is a far
	// gentler state than seed's used to be (the consensus branch is untouched,
	// so nothing is stranded and no later create is blocked: re-running the
	// same create simply adopts the agent branch it finds), but it is still not
	// guessable from a bare "persist origin: …", and this is the last frame
	// that still knows a push happened.
	explainPushed := func(err error) error {
		if spec.Mode != "initialize" || !spec.hasRemote() {
			return err
		}
		return agentBranchAlreadyPushed(err, spec.Origin.URL, m.deps.AgentBranch, resolvedUpstream)
	}

	if cerr := ctx.Err(); cerr != nil {
		cleanup()
		return nil, explainPushed(cerr)
	}

	var originRec *Origin
	if spec.hasRemote() {
		emit(Event{Step: "persist-origin", Message: "saving remote config", Pct: 70})
		// The upstream InitFromRemote RESOLVED, never the one requested — see
		// initClone/initInitialize, both of which return it for exactly this reason.
		originRec = &Origin{
			URL:        spec.Origin.URL,
			Branch:     resolvedUpstream,
			AuthMethod: spec.Origin.AuthMethod,
			AuthToken:  spec.Origin.AuthToken,
		}
		if oerr := origins.Set(uid, *originRec); oerr != nil {
			cleanup()
			return nil, explainPushed(fmt.Errorf("persist origin: %w", oerr))
		}
	}

	emit(Event{Step: "register", Message: "registering repo", Pct: 85})

	if aerr := m.Add(spec.Name, uid, dbPath, originRec); aerr != nil {
		cleanup()
		return nil, explainPushed(fmt.Errorf("register repo: %w", aerr))
	}
	ri := m.Get(spec.Name)
	if ri != nil {
		if id := ri.ID(); id != "" {
			if rerr := reg.RecordRepoID(uid, id); rerr != nil {
				// Only a genuine identity collision (another ACTIVE repo already
				// holds this knowledge base) justifies throwing away an
				// already-completed clone/init. Any other error (e.g. a transient
				// SQLite failure) leaves repo_id unset, which openRegistered simply
				// retries on the next boot (manager.go, same pattern) — so warn and
				// keep the repo rather than destroying real work over it.
				if errors.Is(rerr, ErrRepoAlreadyRegistered) {
					m.Remove(spec.Name)
					cleanup()
					return nil, explainPushed(rerr)
				}
				log.Warn().Err(rerr).Str("repo", spec.Name).Str("uid", uid).
					Msg("recording repo identity failed; repo stays registered")
			}
		}
	}

	if spec.hasRemote() && ri != nil {
		emit(Event{Step: "sync", Message: "activating sync", Pct: 95})
		if serr := ri.ActivateSync(spec.Origin.URL); serr != nil {
			log.Warn().Err(serr).Str("repo", spec.Name).Msg("create: activate sync failed")
		}
	}

	emit(Event{Step: "done", Message: "repo ready", Pct: 100})
	return ri, nil
}

// initLocal handles preset/custom modes: resolve ontology bytes, seed a fresh repo.
func (m *Manager) initLocal(ctx context.Context, spec CreateSpec, dbPath string, emit func(Event)) error {
	emit(Event{Step: "ontology", Message: "resolving ontology", Pct: 20})
	ont, err := resolveOntology(spec)
	if err != nil {
		return err
	}
	y, err := ont.Serialize()
	if err != nil {
		return fmt.Errorf("serialize ontology: %w", err)
	}
	emit(Event{Step: "init-git", Message: "initialising git store", Pct: 50})
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	svc, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer svc.Close()
	svc.SetNetworkTimeout(m.deps.Cfg.Git.NetworkTimeout)
	svc.SetOntologyRoot(m.deps.Cfg.OntologyRoot)
	if err := svc.InitRepo(map[string]string{
		OntologyPath: string(y),
	}, m.deps.AgentBranch); err != nil {
		return fmt.Errorf("init git: %w", err)
	}
	return nil
}

// resolveOntology turns a spec's ontology fields into an Ontology. Shared by
// initLocal and initInitialize so the two modes cannot drift in how they read
// the same two fields.
//
// Mode-aware, not merely field-aware, to reproduce initLocal's ORIGINAL
// precedence byte-for-byte (a bare "OntologyYAML != "" wins" precedence,
// tried first, silently changed two reachable behaviours: a preset request
// that also carried a leftover ontology_yaml value would start parsing the
// YAML instead of honouring the preset, and mode=custom with an empty
// ontology_yaml — previously a hard, surfaced ParseOntology error — would
// silently fall through to the default ontology instead):
//
//   - "custom" ALWAYS parses OntologyYAML, unconditionally, even when it is
//     empty — that must stay a hard error, never a silent default.
//   - "initialize" parses OntologyYAML only when the caller actually supplied
//     one; unlike "custom" it may legitimately carry a preset instead, and
//     initInitialize's own authoritative check (mirroring
//     rejectOntologySpecForClone) independently refuses an initialize request
//     with neither field, so an empty OntologyYAML here is not itself an error
//     — it just means "prefer the preset arm below".
//   - every other mode (preset) ignores OntologyYAML entirely, exactly as the
//     original switch did by gating that arm on `Mode == "custom"` rather
//     than on the field alone.
func resolveOntology(spec CreateSpec) (*fact.Ontology, error) {
	if spec.Mode == "custom" || (spec.Mode == "initialize" && spec.OntologyYAML != "") {
		o, err := fact.ParseOntology([]byte(spec.OntologyYAML))
		if err != nil {
			return nil, fmt.Errorf("parse ontology: %w", err)
		}
		return o, nil
	}
	if spec.OntologyPreset != "" {
		return fact.OntologyByPreset(spec.OntologyPreset)
	}
	return fact.DefaultOntology(), nil
}

// rejectOntologySpecForClone refuses a clone request that also names an
// ontology. A clone's ontology comes from the origin — InitFromRemote overwrites
// the seed files whenever the remote has branches — so honouring a preset here
// would apply it only for an EMPTY origin and silently drop it otherwise. A
// request that is obeyed half the time is worse than one that is refused, and
// the caller learns immediately instead of discovering the default ontology
// later.
func rejectOntologySpecForClone(spec CreateSpec) error {
	if spec.OntologyPreset != "" || spec.OntologyYAML != "" {
		return fmt.Errorf("%w: clone mode takes its ontology from the origin; ontology_preset/ontology_yaml are not accepted", ErrInvalidName)
	}
	return nil
}

// initClone handles clone mode — JOINING a remote that is already a knomit
// knowledge base. It does NOT persist the origin anywhere — Create does that,
// into control.db, once initClone returns.
//
// The returned upstream is the branch the clone ACTUALLY adopted, resolved by
// svc.InitFromRemote against the remote (prefer "main", else its symbolic
// HEAD) whenever spec.Origin.Branch is empty. Create MUST persist exactly this
// value, never the requested spec.Origin.Branch and never a defaulted "main":
// this repo's local branch and fetch refspecs were built from the RESOLVED
// branch, so persisting anything else writes an origin that disagrees with
// them, and every later sync reads a nonexistent origin/<branch>.
//
// A remote that turns out NOT to be a knowledge base is REFUSED here, and that
// refusal is the whole reason this function ends with a check rather than a
// return — see the comment on it below.
func (m *Manager) initClone(ctx context.Context, spec CreateSpec, dbPath string, emit func(Event)) (string, error) {
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	// The authoritative copy of the CreatePreflight check: Create is also called
	// directly (tests, future CLI paths) and the clone below would otherwise
	// quietly ignore a requested ontology.
	if err := rejectOntologySpecForClone(spec); err != nil {
		return "", err
	}
	emit(Event{Step: "clone", Message: "cloning from " + spec.Origin.URL, Pct: 40})
	auth, err := m.ResolveAuth(authConfigFromSpec(spec.Origin), spec.Origin.URL)
	if err != nil {
		return "", fmt.Errorf("resolve auth: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return "", err
	}
	svc, err := store.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer svc.Close()
	svc.SetNetworkTimeout(m.deps.Cfg.Git.NetworkTimeout)
	svc.SetOntologyRoot(m.deps.Cfg.OntologyRoot)
	// No Crypt is wired here: the clone's credential is already resolved above
	// (ResolveAuth) and its durable copy belongs to control.db's Origins, which
	// holds the only Crypt. This store never stores a credential of its own.

	// NO seed files. This used to pass fact.DefaultOntology() so that cloning an
	// EMPTY origin produced a repo with some ontology rather than none — a
	// corner that no longer exists, because an empty remote is refused before
	// any mode runs (initInitialize's ErrRemoteNoBranches). Passing them now
	// would be worse than useless: InitFromRemote writes them ONLY on the
	// empty-remote path, so the one case they could still reach is the one case
	// we refuse.
	//
	// remoteWasEmpty is checked rather than discarded for the same reason. The
	// preflight probe ran strictly EARLIER in time; this flag is what
	// InitFromRemote found at the moment it actually fetched, and a remote that
	// lost its refs in between must not be silently turned into a fresh local
	// knowledge base with a minted identity nobody else shares.
	upstream, remoteWasEmpty, err := svc.InitFromRemote(spec.Origin.URL, auth, spec.Origin.Branch, m.deps.AgentBranch, nil)
	if err != nil {
		return "", fmt.Errorf("clone: %w", err)
	}
	if remoteWasEmpty {
		return "", fmt.Errorf("clone: %w", ErrRemoteNoBranches)
	}
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}

	// THE CHECK THAT MAKES THIS MODE HONEST: a repository is a knomit knowledge
	// base if and only if it has an ontology (fact.OntologyPathsNewestFirst).
	// Clone mode JOINS one, and refuses a supplied ontology on the grounds that
	// the remote's own governs — so a remote with no ontology leaves the mode
	// with nothing to honour.
	//
	// Until this existed the clone SUCCEEDED there, and the missing ontology was
	// silently replaced by fact.DefaultOntology() at the repo's next open
	// (repoBuilder.loadOntology). The user who created their project with a
	// README, was routed here because that made it non-empty, and picked "Code"
	// on the way, got "General" — permanently, since the ontology is immutable
	// after creation, and with nothing in the UI ever saying so.
	//
	// Refusing at CREATE is what closes it. loadOntology's fallback stays as it
	// is: that is the open path, it serves repos that already exist, and hard-
	// failing there would strand exactly the users this bug already hurt.
	hasOnt, oerr := branchHasOntology(ctx, svc, m.deps.AgentBranch)
	if oerr != nil {
		return "", fmt.Errorf("clone: check for an ontology: %w", oerr)
	}
	if !hasOnt {
		return "", fmt.Errorf("clone %s (branch %s): %w", spec.Origin.URL, upstream, ErrRemoteNotInitialized)
	}
	return upstream, nil
}

// initInitialize handles "initialize" mode: turn a branch that is NOT yet a
// knomit knowledge base into one, by writing the caller's chosen ontology onto
// knomit's own agent branch — cut from that branch's tip — and pushing THAT
// BRANCH ALONE.
//
// It is the replacement for the deleted "seed" mode, and the difference is the
// entire point of the redesign. Seed required a completely EMPTY remote, minted
// a root commit, created the consensus branch, and pushed it. That consensus
// push was the only one in this codebase, and it was the sole reason knomit ever
// needed write access to a protected branch — which hosts protect by default on
// new projects, so the create failed outright for anyone below Maintainer:
//
//	seed: push main: pre-receive hook declined
//	GitLab: You cannot push the initial commit because the default branch is
//	        protected and your role does not allow it.
//
// This mode never touches the consensus branch. The user creates the repository
// on their host with a "main" (one commit is enough — "add a README" is just the
// quickest way to get one), and knomit writes only where it is entitled to. The
// knowledge base is backed up from the first write, and the merge request from
// agent/<host> into main is how it later becomes the project's consensus.
//
// Three further consequences follow from cutting from an EXISTING commit rather
// than minting one:
//
//   - Identity is stable across machines. The repo id is the remote's existing
//     root commit, so two machines initializing the same remote agree. Seed
//     minted a nonce per machine, which is the accepted split-brain race
//     documented at store/repo.go's initFromEmptyRemote.
//   - There is no half-written remote. One push instead of two, so seed's window
//     — consensus lands, agent push fails, cleanup deletes the only local copy —
//     cannot arise.
//   - A failed create is retryable. The consensus branch is untouched, so
//     nothing about the remote has been made unusable.
//
// Like initClone, this does NOT persist the origin anywhere — Create does that,
// into control.db, once this returns the resolved upstream. Unlike initClone,
// this DOES push before returning: store.InitFromRemote only ever writes
// locally, so without it the ontology would sit on a local branch the remote has
// never heard of, and the "backed up from the first write" promise would be
// false.
func (m *Manager) initInitialize(ctx context.Context, spec CreateSpec, dbPath string, emit func(Event)) (string, error) {
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	// The authoritative copy of the CreatePreflight check: Create is also
	// called directly (tests, future CLI paths), and resolveOntology below
	// would otherwise quietly default rather than refuse — the exact
	// silent-default this mode exists to prevent. Mirrors initClone's own
	// authoritative re-assertion of rejectOntologySpecForClone, just above.
	if spec.OntologyPreset == "" && spec.OntologyYAML == "" {
		return "", fmt.Errorf("%w: initialize mode requires ontology_preset or ontology_yaml", ErrInvalidName)
	}
	emit(Event{Step: "probe", Message: "checking " + spec.Origin.URL, Pct: 10})
	// Refs only, for the same reason as CreatePreflight: this reads Empty and
	// Branches, never WriteAccess.
	probe, err := m.ProbeOriginRefs(ctx, *spec.Origin)
	if err != nil {
		return "", err
	}
	if serr := initializeProbeErr(probe); serr != nil {
		return "", serr
	}

	emit(Event{Step: "ontology", Message: "resolving ontology", Pct: 20})
	ont, err := resolveOntology(spec)
	if err != nil {
		return "", err
	}
	y, err := ont.Serialize()
	if err != nil {
		return "", fmt.Errorf("serialize ontology: %w", err)
	}

	auth, err := m.ResolveAuth(authConfigFromSpec(spec.Origin), spec.Origin.URL)
	if err != nil {
		return "", fmt.Errorf("resolve auth: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return "", err
	}
	svc, err := store.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer svc.Close()
	svc.SetNetworkTimeout(m.deps.Cfg.Git.NetworkTimeout)
	svc.SetOntologyRoot(m.deps.Cfg.OntologyRoot)
	// No Crypt is wired here, for the same reason initClone doesn't: the
	// credential is already resolved above, and its durable copy belongs to
	// control.db's Origins, which holds the only Crypt.

	// nil initFiles, exactly as initClone passes: InitFromRemote writes them
	// only on the EMPTY-remote path, which is the path this mode refuses. The
	// ontology is written below instead — as an ordinary commit on the agent
	// branch, through the same fact machinery every later write uses.
	emit(Event{Step: "clone", Message: "reading " + spec.Origin.URL, Pct: 40})
	upstream, remoteWasEmpty, err := svc.InitFromRemote(spec.Origin.URL, auth, spec.Origin.Branch, m.deps.AgentBranch, nil)
	if err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	// THE AUTHORITATIVE branch check. initializeProbeErr above is a fail-fast
	// affordance run against a probe taken strictly EARLIER in time; this flag
	// is InitFromRemote's report of what it found at the moment it actually
	// fetched. A remote that lost its refs in that window took the empty path,
	// which mints a fresh root commit and therefore a repo identity no other
	// machine shares — the split-brain seed mode used to accept. Refuse it.
	if remoteWasEmpty {
		return "", fmt.Errorf("initialize: %w (it had no refs at fetch time)", ErrRemoteNoBranches)
	}

	// Refuse a branch that is ALREADY a knowledge base rather than writing a
	// second ontology over the one that governs it — the mirror of initClone's
	// check, and load-bearing for the same reason: the ontology is immutable
	// after creation, so a wrong one here is not correctable later.
	//
	// It is checked on the AGENT branch, not the consensus branch, because that
	// is where the write would land. The two differ in one reachable case: this
	// machine initialized the remote before, never merged, and is now creating
	// again — InitFromRemote adopts its existing origin/agent/<host>, which
	// already carries an ontology. The wizard looked at the consensus branch and
	// saw none, so it offered this mode; refusing here with "use clone" is
	// correct, and clone genuinely succeeds, because it runs this same check
	// against the same adopted branch.
	hasOnt, oerr := branchHasOntology(ctx, svc, m.deps.AgentBranch)
	if oerr != nil {
		return "", fmt.Errorf("initialize: check for an ontology: %w", oerr)
	}
	if hasOnt {
		return "", fmt.Errorf("initialize %s (branch %s): %w", spec.Origin.URL, upstream, ErrRemoteAlreadyInitialized)
	}

	// THE ACT that makes this a knowledge base. One ordinary commit on the agent
	// branch, through the same fact machinery every later write uses — not a
	// special-cased root commit, which is what let seed's identity diverge.
	emit(Event{Step: "ontology-write", Message: "writing " + OntologyPath, Pct: 55})
	if _, werr := svc.Facts().WriteFact(ctx, m.deps.AgentBranch, OntologyPath, string(y),
		"init: create knowledge base", "created"); werr != nil {
		return "", fmt.Errorf("initialize: write %s: %w", OntologyPath, werr)
	}

	// THE AGENT BRANCH ONLY. Steady-state sync pushes exactly this ref and no
	// other (repos/sync.go), and this bootstrap deliberately does not become the
	// exception — pushing the consensus branch here would reintroduce the
	// protected-branch failure this mode was built to remove.
	emit(Event{Step: "push", Message: "pushing " + m.deps.AgentBranch + " to " + spec.Origin.URL, Pct: 70})
	// The PushResult is inspected rather than discarded: Push reports
	// Pushed:false for "nothing to push", which right after a commit this
	// function just made is a silent no-op, not a success. Left unchecked it is
	// indistinguishable from a real push, and Create would go on to report a
	// knowledge base the remote has never seen.
	agentPush, perr := svc.Remote().Push(ctx, m.deps.AgentBranch, auth)
	if perr != nil {
		// The remote is UNCHANGED — nothing was pushed — so this is the one
		// failure in this mode that needs no state explanation, only its cause.
		// Naming the branch matters though: a host that refuses this push is
		// refusing agent/<host>, not the consensus branch the user was probably
		// worrying about.
		return "", fmt.Errorf("initialize: push %s: %w", m.deps.AgentBranch, perr)
	}
	if !agentPush.Pushed {
		return "", fmt.Errorf(
			"initialize: push %s: remote reported nothing to push, so %s was left without the ontology commit",
			m.deps.AgentBranch, spec.Origin.URL)
	}

	// Past the push, so a cancellation here leaves the agent branch on the
	// remote while Create rolls the local repo back. The Create-side wrapper
	// never sees this one — the mode switch returns it directly — so it carries
	// its own explanation.
	if cerr := ctx.Err(); cerr != nil {
		return "", agentBranchAlreadyPushed(cerr, spec.Origin.URL, m.deps.AgentBranch, upstream)
	}
	return upstream, nil
}

// agentBranchAlreadyPushed annotates a failure that happens AFTER initialize's
// push has landed. Every such path — initInitialize's trailing cancellation
// check and each rollback in Create past the mode switch — goes through here,
// so the user gets one description of the state they are in rather than one
// path that explains itself and three that do not.
//
// The state it describes is deliberately mild, and says so: the consensus
// branch was never touched, so the remote is not stranded and re-running the
// same create simply adopts the agent branch it finds. That is the whole
// improvement over the seed-era version of this message, which had to explain
// an unrecoverable half-seeded remote and offer two ways out of it.
//
// It wraps with %w: ErrRepoAlreadyRegistered and context.Canceled both reach
// this, and both are matched with errors.Is by callers (createErrStatus in
// internal/web, among others).
func agentBranchAlreadyPushed(err error, url, agentBranch, upstream string) error {
	return fmt.Errorf("%w (%s was already pushed to %s, so the ontology is safe there; %s was not touched, "+
		"and creating this repository again will adopt that branch rather than start over)",
		err, agentBranch, url, upstream)
}

// initializeProbeErr maps a ProbeOrigin result to the error initInitialize
// should return before doing any work, or nil when the remote is a valid
// initialize target.
//
// AuthRequired is checked BEFORE Empty deliberately: ProbeOrigin reports an
// auth-required remote as {Reachable:true, AuthRequired:true, Empty:false}
// (probe.go) — it has no way to know whether a remote it cannot authenticate
// against has branches or not. Checking Empty first would be reading a flag the
// probe never established.
//
// The Empty arm is the INVERSE of the seed-era check it replaces: a remote with
// no refs has no branch to cut the agent branch from, and knomit never creates
// one on a remote other than its own.
func initializeProbeErr(probe ProbeResult) error {
	if !probe.Reachable {
		return fmt.Errorf("initialize: remote not reachable: %s", probe.Detail)
	}
	if probe.AuthRequired {
		return fmt.Errorf("initialize: remote requires authentication: %s", probe.Detail)
	}
	// Re-asserted HERE, not trusted from the client: the wizard's probe and
	// this create are separated in time, and a remote can lose refs in between.
	if probe.Empty {
		return ErrRemoteNoBranches
	}
	return nil
}

// authConfigFromSpec maps an OriginSpec to the config shape ResolveAuth expects.
// For basic auth the token field carries "user:password" (the same convention
// remoteAuthFromRecord uses when reading a persisted basic remote), so it is
// split into User/Password here; otherwise the immediate clone would attempt
// basic auth with an empty username and fail against real hosts.
func authConfigFromSpec(o *OriginSpec) config.RemoteAuthConfig {
	cfg := config.RemoteAuthConfig{
		Token:      o.AuthToken,
		Password:   o.AuthToken,
		AuthMethod: o.AuthMethod,
	}
	if o.AuthMethod == "basic" {
		if user, pass, ok := strings.Cut(o.AuthToken, ":"); ok {
			cfg.User = user
			cfg.Password = pass
		}
	}
	return cfg
}

// ActiveRepoWithOrigin returns the name of an active repo whose origin URL
// equals url, or "" if none.
//
// A cheap PREFLIGHT that fails before any network fetch, not the real
// uniqueness guard: a mirror of the same repository has a different URL and
// passes here. Identity uniqueness is Registry.RecordRepoID's job, enforced
// after the clone when the root commit is known.
func (m *Manager) ActiveRepoWithOrigin(url string) string {
	origins := m.Origins()
	if origins == nil {
		return ""
	}
	name, err := origins.ActiveRepoWithURL(url)
	if err != nil {
		log.Warn().Err(err).Msg("origin uniqueness check failed; allowing the operation to proceed to the identity check")
		return ""
	}
	return name
}

// ArchiveInfo describes one archived repo. ID is the repo's uid — the same
// identity Create minted and the same key Restore/Purge take.
type ArchiveInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Origin     string `json:"origin"`
	ArchivedAt string `json:"archivedAt"`
	// SizeBytes is the archived database's on-disk size. Archived databases are
	// no longer visible under an obvious filename (there is no repos/archive/
	// directory to `ls`), so this is how reclaimable disk stays visible.
	SizeBytes int64 `json:"sizeBytes"`
}

// Archive shuts the named repo down and flips its registry state. The database
// file never moves.
//
// The only fallible step (the UPDATE) runs BEFORE the map delete, so a failure
// returns having changed nothing — there is no rollback path to get wrong. The
// state flip happens under the same m.mu that CreateLens/UpdateLens hold, which
// is what keeps a lens member from being archived between its membership check
// and its persist (the P1 property).
//
// ANY repo may be archived, including the last one: no repo is privileged, and
// zero repos is a valid state.
func (m *Manager) Archive(name string) (ArchiveInfo, error) {
	// Truncated to the second because that is the resolution the registry
	// stores (SetState takes a Unix second) and therefore the resolution
	// ListArchived renders back. Formatting the untruncated value here would
	// make the archive response and the very next listing disagree about when
	// the same record was archived — a client keyed on archivedAt would see two
	// timestamps for one event. Same reason SizeBytes is stat'd after shutdown:
	// this response is the last word on the repo, and it must agree with the
	// listing that follows it.
	now := time.Now().UTC().Truncate(time.Second)

	// Snapshot BEFORE the lock: controlHandles takes m.mu.RLock, and RWMutex is
	// not reentrant. The snapshot is also what lets the SetState below stay a
	// plain local call from inside the critical section.
	reg, origins, err := m.controlHandles()
	if err != nil {
		return ArchiveInfo{}, err
	}

	m.mu.Lock()
	ri := m.repos[name]
	if ri == nil {
		m.mu.Unlock()
		return ArchiveInfo{}, fmt.Errorf("%w: %q", ErrRepoNotFound, name)
	}
	uid := ri.uid
	// Direct field read: we already hold the write lock, so the accessor's
	// RLock would deadlock. RefsRepo is keyed by registry UID (lenses store
	// lenses.write_uid / lens_reads.repo_uid, never names — see lens.go), so
	// this must pass uid, not name. Passing the name would match no lens row
	// and silently permit archiving a lens member.
	if m.registry != nil {
		refs, rerr := m.registry.RefsRepo(uid)
		if rerr != nil {
			m.mu.Unlock()
			return ArchiveInfo{}, fmt.Errorf("lens registry: %w", rerr)
		}
		if len(refs) > 0 {
			m.mu.Unlock()
			return ArchiveInfo{}, fmt.Errorf("%w: %q (lenses: %s)", ErrRepoInUseByLens, name, strings.Join(refs, ", "))
		}
	}
	if serr := reg.SetState(uid, StateArchived, now.Unix()); serr != nil {
		m.mu.Unlock()
		return ArchiveInfo{}, fmt.Errorf("archive: %w", serr)
	}
	delete(m.repos, name)
	delete(m.byUID, uid)
	// Unregistering has to drop the unavailable flag too, or a uid that was
	// flagged at boot and later came back would stay flagged for the rest of the
	// process and reappear in GET /repos as a row naming an archived repo the
	// user cannot act on. Spelled as a bare delete rather than clearUnavailable
	// because we already hold m.mu — and doing it here rather than after the
	// unlock leaves no window in which the repo is out of m.repos but still
	// listed as unavailable.
	delete(m.unavailable, uid)
	m.mu.Unlock()

	var origin string
	if org, oerr := origins.Get(uid); oerr == nil && org != nil {
		origin = org.URL
	}

	ri.shutdown() // releases the SQLite file handle

	// The session sidecar is ephemeral — drop it so a restore starts clean.
	sess := store.SessionDBPathFor(m.RepoPath(uid))
	os.Remove(sess)
	os.Remove(sess + "-wal")
	os.Remove(sess + "-shm")

	log.Info().Str("repo", name).Str("uid", uid).Msg("archived repo")
	info := ArchiveInfo{
		ID:         uid,
		Name:       name,
		Origin:     origin,
		ArchivedAt: now.Format(time.RFC3339Nano),
	}
	// Stat AFTER shutdown, so the WAL has been folded back into the file and the
	// size is the one ListArchived will report for the same repo a moment later.
	// Same reason ListArchived carries it: the archive response is the last time
	// the caller is told anything about this repo, and it must not be the one
	// shape that omits how much disk it is still holding.
	if st, serr := os.Stat(m.RepoPath(uid)); serr == nil {
		info.SizeBytes = st.Size()
	}
	return info, nil
}

// ListArchived returns every archived repo, newest first.
func (m *Manager) ListArchived() ([]ArchiveInfo, error) {
	reg, origins, err := m.controlHandles()
	if err != nil {
		return nil, err
	}
	recs, err := reg.List(StateArchived)
	if err != nil {
		return nil, err
	}
	out := make([]ArchiveInfo, 0, len(recs))
	for _, rec := range recs {
		var origin string
		if org, oerr := origins.Get(rec.UID); oerr == nil && org != nil {
			origin = org.URL
		}
		info := ArchiveInfo{
			ID:     rec.UID,
			Name:   rec.Name,
			Origin: origin,
		}
		if rec.ArchivedAt != 0 {
			info.ArchivedAt = time.Unix(rec.ArchivedAt, 0).UTC().Format(time.RFC3339Nano)
		}
		// Archived databases are no longer visible under an obvious filename, so
		// report their size: otherwise reclaimable disk is invisible.
		if st, serr := os.Stat(m.RepoPath(rec.UID)); serr == nil {
			info.SizeBytes = st.Size()
		}
		out = append(out, info)
	}
	// Newest first, then uid ascending — a TOTAL order, not merely a primary
	// key. archived_at is stored as a Unix SECOND, so two repos archived in the
	// same second compare equal on the timestamp; without the tiebreak (and
	// with sort.Slice, which is not stable) two calls over one registry are free
	// to return them in different orders. ID is the record's uid.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ArchivedAt != out[j].ArchivedAt {
			return out[i].ArchivedAt > out[j].ArchivedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Restore re-activates an archived repo, optionally under newName to resolve a
// name collision. The database file was never moved, so this is a state flip
// plus an open.
func (m *Manager) Restore(uid, newName string) (*RepoInstance, error) {
	reg, origins, err := m.controlHandles()
	if err != nil {
		return nil, err
	}
	rec, ok, err := reg.Get(uid)
	if err != nil {
		return nil, err
	}
	if !ok || rec.State != StateArchived {
		return nil, fmt.Errorf("%w: %q", ErrArchiveNotFound, uid)
	}
	target := rec.Name
	if newName != "" {
		target = newName
	}
	if !isValidRepoName(target) {
		return nil, ErrInvalidName
	}

	var originURL string
	origin, oerr := origins.Get(uid)
	if oerr != nil {
		return nil, fmt.Errorf("restore: read origin: %w", oerr)
	}
	if origin != nil {
		originURL = origin.URL
	}

	// Reserve the target name and the archived origin for the whole check → Add
	// window so a concurrent Create/Restore can't race us to register the same
	// name or attach the same origin to two repos. reserveNameAndOrigin also
	// performs the authoritative active-origin scan.
	release, err := m.reserveNameAndOrigin(target, originURL)
	if err != nil {
		return nil, err
	}
	defer release()

	if m.Get(target) != nil {
		return nil, fmt.Errorf("%w: %q", ErrRepoExists, target)
	}
	// An archived repo's name can be claimed by a lens while it sits archived,
	// so restoring must refuse a name a lens now holds.
	if err := m.lensNameConflict(target); err != nil {
		return nil, err
	}

	if target != rec.Name {
		if rerr := reg.Rename(uid, target); rerr != nil {
			return nil, rerr // ErrRepoExists when an active repo holds the name
		}
	}
	if serr := reg.SetState(uid, StateActive, 0); serr != nil {
		if target != rec.Name {
			_ = reg.Rename(uid, rec.Name)
		}
		return nil, serr // ErrRepoAlreadyRegistered when its identity is taken
	}

	if aerr := m.Add(target, uid, m.RepoPath(uid), origin); aerr != nil {
		// Put it back: the file is untouched, so recovery is two UPDATEs.
		_ = reg.SetState(uid, StateArchived, rec.ArchivedAt)
		if target != rec.Name {
			_ = reg.Rename(uid, rec.Name)
		}
		return nil, fmt.Errorf("restore register: %w", aerr)
	}

	ri := m.Get(target)

	// Record which knowledge base this repo holds. The invariant "an ACTIVE
	// repo's repo_id is recorded" is established in exactly three places —
	// Create (above), Start's openRegistered (manager.go), and SwapStore
	// (swapstore.go) — and Restore is the fourth first-open there is: an
	// archived repo that was archived before repo_id existed, or registered
	// with a NULL one by migrate-registry, has its FIRST successful open right
	// here.
	//
	// Leaning on SetState to catch a conflict is not enough. The
	// repos_active_repo_id index is WHERE state='active' AND repo_id IS NOT
	// NULL, so a NULL row flips to active unchallenged and STAYS null —
	// silently disarming heldByAnotherActiveRepo for that repo from then on.
	if ri != nil {
		if id := ri.ID(); id != "" {
			if rerr := reg.RecordRepoID(uid, id); rerr != nil {
				if errors.Is(rerr, ErrRepoAlreadyRegistered) {
					// Another ACTIVE repo already holds this knowledge base.
					// Two live copies both write agent/<host> and clobber each
					// other on push, so undo the restore rather than leave the
					// second one running. The file is untouched, so this is a
					// detach and two UPDATEs.
					m.Remove(target)
					_ = reg.SetState(uid, StateArchived, rec.ArchivedAt)
					if target != rec.Name {
						_ = reg.Rename(uid, rec.Name)
					}
					return nil, rerr
				}
				// Any other error (a transient SQLite failure) leaves repo_id
				// unset, which the next boot's openRegistered simply retries —
				// so warn and keep the restored repo rather than undoing it.
				log.Warn().Err(rerr).Str("repo", target).Str("uid", uid).
					Msg("restore: recording repo identity failed; repo stays restored")
			}
		}
	}

	if ri != nil && originURL != "" {
		if serr := ri.ActivateSync(originURL); serr != nil {
			log.Warn().Err(serr).Str("repo", target).Msg("restore: activate sync failed")
		}
	}
	log.Info().Str("uid", uid).Str("repo", target).Msg("restored repo")
	return ri, nil
}

// Purge permanently deletes an archived repo: its registry row (cascading to
// its stored credential) and then its database file.
//
// Row first, then file, deliberately. A failed unlink leaves an orphan file —
// logged at next Start, harmless, deletable by hand. The reverse order would
// leave a row pointing at nothing, which presents as a MISSING repo offering to
// rehydrate itself: the worst outcome for an operation meaning "destroy this".
func (m *Manager) Purge(uid string) error {
	reg, _, err := m.controlHandles()
	if err != nil {
		return err
	}
	rec, ok, err := reg.Get(uid)
	if err != nil {
		return err
	}
	if !ok || rec.State != StateArchived {
		return fmt.Errorf("%w: %q", ErrArchiveNotFound, uid)
	}
	// RefsRepo is keyed by registry UID (see the comment in Archive), so this
	// checks rec.UID, not rec.Name. The error still NAMES the repo — that is
	// what the operator typed — but the lookup is by uid.
	if lensReg := m.LensRegistry(); lensReg != nil {
		refs, rerr := lensReg.RefsRepo(rec.UID)
		if rerr != nil {
			return fmt.Errorf("lens registry: %w", rerr)
		}
		if len(refs) > 0 {
			return fmt.Errorf("%w: %q (lenses: %s)", ErrRepoInUseByLens, rec.Name, strings.Join(refs, ", "))
		}
	}
	if derr := reg.Delete(uid); derr != nil {
		return derr
	}
	m.clearUnavailable(uid)
	db := m.RepoPath(uid)
	if rerr := os.Remove(db); rerr != nil && !os.IsNotExist(rerr) {
		log.Error().Err(rerr).Str("uid", uid).
			Msg("purge: registry row removed but the database file remains; delete it by hand")
	}
	os.Remove(db + "-wal")
	os.Remove(db + "-shm")
	log.Info().Str("uid", uid).Str("repo", rec.Name).Msg("purged repo")
	return nil
}

// RenameRepo changes a repo's display name. The store is NOT closed: a name is
// display-only (the .db is <home>/repos/<uid>.db, lens membership is uid-keyed,
// and the lens wire format derives its name field), so this is a control.db
// UPDATE plus a map-key move. Remove+Add — what Restore does — would call
// ri.shutdown(), closing the store, dropping SSE subscribers and forcing an
// index re-warm, all to change a string.
//
// A rename used to have a consequence here: the MCP cursor-pinning identity
// (lenses RFC §7.3), persisted into tool_sessions.binding, was Binding.Name()
// — so renaming a repo orphaned any in-flight knomit_query/knomit_explain
// session pinned to the old name. That pin is now Binding.PinID()
// (repo:<uid>), which this call never touches, so an in-flight cursor
// survives a rename.
//
// Rejects a name that is invalid, held by another ACTIVE repo, or held by a
// lens. Renaming to the current name is a successful no-op.
//
// reserveNameAndOrigin excludes this from Create/Restore/CreateLens racing the
// SAME newName, but it reserves only the new name — it does not exclude
// Archive or Remove (neither consults it, or the old name, at all), and it
// does not exclude a second concurrent RenameRepo of this SAME oldName to a
// DIFFERENT newName (the two reservations don't collide because the target
// names differ). An unconditional Registry.Rename has no state predicate, so
// left unguarded either race would durably rename an archived row or resurrect
// a shut-down instance into the active map — or leave the registry disagreeing
// with whichever instance actually kept the name in memory.
//
// Two separate mechanisms close these, both built on the same conditional
// primitive (RenameIfNamed) rather than the unconditional Registry.Rename that
// Restore still uses for its forward write and its compensations (grep
// `reg.Rename(` in this file — deliberately not cited by line number, which has
// already drifted twice):
//
//  1. The forward write is itself a CAS — RenameIfNamed(oldName, newName) —
//     not a bare Rename. SQLite serializes concurrent writers, so of two
//     RenameRepo calls racing FROM the same oldName, at most one CAS can see
//     oldName still there and succeed. The loser learns it lost right here, at
//     its own forward write, and never reaches the code below — so IT has
//     certainly touched no map. (The winner may already have swapped the map by
//     then; the point is not that both are still pre-map, it is that the loser
//     stops before writing anything and therefore has nothing to compensate.)
//     This is what actually resolves two-different-targets races — the
//     revalidate step alone cannot, because by the time a loser would reach
//     it, an unconditional forward write from EITHER call could already have
//     overwritten the other's, independent of which one goes on to win the
//     in-memory map.
//  2. The revalidate-under-the-lock step below catches what the forward CAS
//     cannot see: Archive/Remove drop oldName from the map WITHOUT touching
//     its registry row's name column, so this call's own forward CAS can
//     still succeed against an oldName that Archive/Remove pulled out of
//     m.repos moments earlier. When that happens the map is left untouched
//     and this call's own durable write is undone with
//     RenameIfNamed(newName, oldName) — conditional again, so it only ever
//     undoes THIS call's write and can't clobber a legitimate concurrent
//     rename of the same uid. A revert that reports no row changed means a
//     racer legitimately holds the name now, which is correct, not an error;
//     a revert that fails outright leaves the registry disagreeing with the
//     map until the next boot, which is why that case is logged rather than
//     discarded.
func (m *Manager) RenameRepo(oldName, newName string) error {
	if !isValidRepoName(newName) {
		return ErrInvalidName
	}
	// Snapshot the control handles BEFORE taking m.mu: controlHandles takes
	// m.mu.RLock and RWMutex is not reentrant. Same ordering as Archive.
	reg, _, err := m.controlHandles()
	if err != nil {
		return err
	}

	ri := m.Get(oldName)
	if ri == nil {
		return fmt.Errorf("%w: %q", ErrRepoNotFound, oldName)
	}
	if oldName == newName {
		return nil
	}

	// Hold the target name for the whole check → commit window so a concurrent
	// Create or Restore cannot claim it between the checks and the UPDATE.
	release, err := m.reserveNameAndOrigin(newName, "")
	if err != nil {
		return err
	}
	defer release()

	if m.Get(newName) != nil {
		return fmt.Errorf("%w: %q", ErrRepoExists, newName)
	}
	// A lens may hold the name even though no repo does; repos and lenses share
	// one namespace. Same guard Create and Restore run.
	if err := m.lensNameConflict(newName); err != nil {
		return err
	}

	// Conditional, not unconditional: an unconditional forward write here is
	// racy against a second concurrent RenameRepo of this SAME oldName to a
	// DIFFERENT target. Both calls would otherwise write the row in turn with
	// no ordering relative to which one goes on to win the in-memory map swap
	// below, so the registry could end up holding EITHER target's name
	// regardless of which one actually keeps the map entry — an unconditional
	// write plus a merely-conditional compensation (an earlier version of this
	// fix) is not enough, because the compensation only ever undoes a call's
	// OWN write; it does nothing about a straggling forward write from the
	// call that loses the map race landing chronologically AFTER the winner's.
	// RenameIfNamed(oldName, newName) commits only if the row still holds
	// oldName, and SQLite serializes concurrent writers, so of two renames
	// racing FROM the same oldName at most one forward commit can ever
	// succeed — the loser learns it lost right here, at its own forward write,
	// and never gets far enough to touch the map or to need compensating. (The
	// WINNER may already have swapped the map by then; what matters is that the
	// loser wrote nothing.)
	changed, err := reg.RenameIfNamed(ri.UID(), oldName, newName)
	if err != nil {
		return err // ErrRepoExists when an active repo raced us to the name
	}
	if !changed {
		// oldName moved before this call's write landed — a racing rename won,
		// or oldName was already stale. Nothing durable was written by this
		// call, so there is nothing to undo.
		return fmt.Errorf("%w: %q", ErrRepoNotFound, oldName)
	}

	// Revalidate under the SAME lock that performs the swap. The forward CAS
	// above closes the same-oldName race, but it says nothing about Archive or
	// Remove, which drop oldName from the map WITHOUT touching its registry
	// row's name column — so this call's forward CAS can still succeed against
	// an oldName that Archive/Remove pulled out of m.repos moments earlier. If
	// oldName no longer maps to this exact instance (or newName has since been
	// taken), the map itself is left untouched, and the durable rename this
	// call just wrote is undone with RenameIfNamed(newName, oldName) — the
	// conditional revert, not an unconditional one, so it only ever undoes
	// THIS call's own write and can never clobber a legitimate concurrent
	// rename of the same uid.
	m.mu.Lock()
	if m.repos[oldName] != ri || m.repos[newName] != nil {
		m.mu.Unlock()
		if reverted, cerr := reg.RenameIfNamed(ri.UID(), newName, oldName); cerr != nil {
			log.Error().Err(cerr).Str("uid", ri.UID()).Str("from", newName).Str("to", oldName).
				Msg("rename: could not revert the registry after a lost race; registry and live map may disagree until restart")
		} else if !reverted {
			log.Info().Str("uid", ri.UID()).Str("held", newName).
				Msg("rename: revert skipped — another operation already changed this repo's name")
		}
		return fmt.Errorf("%w: %q", ErrRepoNotFound, oldName)
	}
	m.repos[newName] = ri
	delete(m.repos, oldName)
	// setName INSIDE this same critical section, not after: Get/Names/ForEach
	// all take m.mu.RLock, so a reader that resolves ri between an out-of-lock
	// setName and the map swap (or vice versa) would observe an instance whose
	// Name() names neither key it is reachable under. setName is a lock-free
	// atomic Store — cheap, and it cannot block or deadlock — so there is no
	// cost to closing that window by publishing the name before releasing mu.
	ri.setName(newName)
	m.mu.Unlock()

	log.Info().Str("uid", ri.UID()).Str("from", oldName).Str("to", newName).Msg("renamed repo")
	return nil
}

// RenameLens changes a lens's display name. lens_reads references lens_uid
// (Task 4b), never a name, so this is a single control.db UPDATE of the
// lenses row and touches no read mount — that uid-keying is the whole point of
// moving lens membership off names.
//
// The in-memory half is materially simpler than RenameRepo above, and for a
// reason worth spelling out rather than assuming: Manager holds NO in-memory
// map of lenses analogous to m.repos/m.byUID. A lens is resolved from
// LensRegistry per request (LensRegistry(), CreateLens, UpdateLens all go
// straight to m.registry), so there is nothing to re-key, no second copy of the
// name that could disagree with the registry row, and therefore none of
// RenameRepo's revalidate-under-the-lock-then-compensate structure is needed
// here: the registry row IS the only place a lens's name lives, so checking it
// and writing it in the same m.mu.Lock() critical section is enough.
//
// That single critical section serializes this against every OTHER lens
// mutation (CreateLens, UpdateLens, and a second RenameLens all hold m.mu for
// their whole duration), and LensRegistry.Rename is still the CAS form
// (WHERE uid = ? AND name = ?) rather than a plain UPDATE, on the same
// reasoning RenameRepo's doc comment gives: a plain UPDATE is the shape that
// let two concurrent repo renames both durably succeed with the last writer
// winning, and there is no reason the lens registry should be one direct call
// away from the identical failure mode should a future caller ever reach it
// outside Manager's lock.
//
// m.mu ALONE IS NOT ENOUGH, and the reservation below is not optional.
// Repo Create/Restore do their slow work — git init, a network clone — WITHOUT
// holding m.mu, and only call m.Add(name) at the very END. So a
// Create("foo") that has already taken its reservation and cleared
// lensNameConflict leaves m.repos["foo"] == nil for the entire clone, and an
// in-lock `m.repos[newName] != nil` check is therefore not evidence that no
// repo is claiming newName — it is only evidence that none has FINISHED
// claiming it. Left unreserved, RenameLens("eng" → "foo") sails through that
// check mid-clone and commits, and the machine ends up with a repo foo AND a
// lens foo, durably, with nothing repairing it at the next boot. The same hole
// is open against Restore and against a concurrent RenameRepo(_ → "foo").
// Reserving newName in the SAME in-flight set (m.creating, via
// reserveNameAndOrigin) closes it exactly as it does for CreateLens (P2, see
// manager.go) and for RenameRepo: whichever side reserves first wins, the other
// gets ErrCreateInFlight, and because the winner releases only after persisting,
// the loser's post-reservation re-check observes it. The `m.repos[newName]`
// read below is that re-check, not the primary guard.
//
// ORDERING: reserveNameAndOrigin must be called BEFORE m.mu.Lock(), never
// while holding it — it can call ActiveRepoWithOrigin, which takes m.mu, and
// sync.RWMutex is not reentrant. The deferred release then runs strictly after
// the deferred m.mu.Unlock() (defers are LIFO), which is the overlap CreateLens
// depends on: reserved while also persisted.
//
// Guards mirror RenameRepo's: newName must pass the shared name grammar
// (ErrInvalidLensName), must not be being claimed by an in-flight
// Create/Restore/CreateLens/rename (ErrCreateInFlight), must not be held by an
// ACTIVE repo (ErrLensNameConflictsRepo — repos and lenses share one namespace,
// gotcha M-1; checked against m.repos directly, the same in-lock read
// validateLensLocked uses), and must not be held by another lens (ErrLensExists,
// via LensRegistry.Rename's CAS hitting the lenses_name UNIQUE index).
// oldName must resolve to an existing lens (ErrLensNotFound). Renaming to the
// current name is a successful no-op.
//
// A rename has no cursor-pinning consequence: the MCP binding pin is
// lens:<uid> (Binding.PinID(), RFC §7.3), never the name, so an in-flight
// knomit_query/knomit_explain session pinned to this lens survives a rename
// exactly as an in-flight repo session survives RenameRepo.
func (m *Manager) RenameLens(oldName, newName string) error {
	if !isValidRepoName(newName) {
		return fmt.Errorf("%w: %q", ErrInvalidLensName, newName)
	}

	// Hold the target name for the whole check → commit window, so a Create or
	// Restore that is mid-clone — reservation taken, m.Add not yet reached, and
	// therefore INVISIBLE to the m.repos read below — cannot end up sharing the
	// name with this lens. Before m.mu.Lock(): reserveNameAndOrigin may take m.mu
	// itself and RWMutex is not reentrant. The deferred release runs after the
	// deferred Unlock (LIFO), so the reservation outlives the persist.
	release, err := m.reserveNameAndOrigin(newName, "")
	if err != nil {
		return err // ErrCreateInFlight when another operation already holds this name
	}
	defer release()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.registry == nil {
		return fmt.Errorf("lens registry not open")
	}

	l, ok, err := m.registry.Get(oldName)
	if err != nil {
		return fmt.Errorf("lens registry: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: %q", ErrLensNotFound, oldName)
	}
	if oldName == newName {
		return nil
	}
	// A repo may hold newName even though no lens does — repos and lenses share
	// one namespace (gotcha M-1). Direct field read: we already hold m.mu, so
	// the Get accessor's RLock would deadlock — the same reasoning Archive's
	// direct m.repos/m.registry reads document above.
	//
	// This is the RE-CHECK, exactly as in RenameRepo: it catches a Create or
	// Restore that finished (m.Add landed) before this call reserved the name.
	// It cannot catch one still in flight — that is what the reservation above
	// is for, and why removing it would reopen the repo/lens name collision.
	if m.repos[newName] != nil {
		return fmt.Errorf("%w: %q", ErrLensNameConflictsRepo, newName)
	}
	// A repo whose .db file is missing or unopenable at boot has no live
	// instance — it never reaches m.repos, it lives in m.unavailable instead
	// (markUnavailable, manager.go) — but its registry row still holds the
	// name, and nothing joins the lenses table to repo names. Without this
	// scan, restoring that file later resurrects a repo sharing a name with
	// this lens, violating gotcha M-1 durably. m.unavailable is keyed by uid,
	// not name, so this must be a linear scan; direct field read for the same
	// reason as m.repos above — we already hold m.mu, and the Unavailable()
	// accessor's RLock would deadlock.
	for _, u := range m.unavailable {
		if u.Record.Name == newName {
			return fmt.Errorf("%w: %q", ErrLensNameConflictsRepo, newName)
		}
	}

	changed, err := m.registry.Rename(l.UID, oldName, newName)
	if err != nil {
		return err // ErrLensExists when another lens already holds newName
	}
	if !changed {
		// Unreachable today (this whole function runs under one m.mu.Lock(), so
		// nothing else can have moved oldName in between) but Rename's CAS
		// reports it rather than silently doing nothing, so a future caller that
		// reaches LensRegistry.Rename outside this lock is told honestly, not
		// left believing a rename happened when it didn't.
		return fmt.Errorf("%w: %q", ErrLensNotFound, oldName)
	}

	log.Info().Str("uid", l.UID).Str("from", oldName).Str("to", newName).Msg("renamed lens")
	return nil
}
