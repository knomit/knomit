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

	"github.com/go-git/go-git/v5/plumbing/transport"
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
	// ErrLensNameConflictsRepo: a lens and a lens-of-one repo both surface
	// Binding.Name() as their cursor-pinning identity (RFC §7.3). A same-name
	// cursor cross-resume is ALSO blocked physically — sessions live in the
	// write repo's own session DB, and a lens can never be named after its own
	// write repo — so this guard is defense-in-depth for the name check plus
	// namespace legibility (one name must never serve two endpoints).
	// ValidateLens closes the direction where the lens is created second; this
	// closes the direction where the repo is created (or restored) second, so
	// gotcha M-1 (kb/gotchas/lens/cursor-binding-pin) is enforced
	// bidirectionally rather than one-way. It is checked in the user-facing
	// name-introducing paths (CreatePreflight, Create, Restore) but deliberately
	// NOT in Manager.Add — see the comment on Add.
	ErrRepoNameConflictsLens = errors.New("repo name conflicts with an existing lens name")
)

// lensNameConflict reports ErrRepoNameConflictsLens when name already exists as
// a lens in the registry, enforcing the reverse direction of the lens/repo
// name-disjointness invariant (gotcha M-1). It returns nil when the registry is
// not yet open (before Start), matching how Archive skips its lens check on a
// nil registry: fail soft at startup, fail loud at creation. Callers are the
// user-facing name-introducing paths (CreatePreflight, Create, Restore) that do
// NOT hold m.mu at their call sites, so the Registry() accessor (which takes
// m.mu.RLock) is safe here — no self-deadlock as in Archive's direct-field read.
func (m *Manager) lensNameConflict(name string) error {
	reg := m.Registry()
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
type CreateSpec struct {
	Name           string
	Mode           string // "preset" | "custom" | "clone"
	OntologyPreset string
	OntologyYAML   string
	Origin         *OriginSpec
}

// Event is a progress message emitted during Create.
type Event struct {
	Step    string `json:"step"`
	Message string `json:"message"`
	Pct     int    `json:"pct"`
}

// CreatePreflight runs the cheap synchronous checks that must surface as an
// HTTP status BEFORE any streaming begins: name validity, clone-origin
// presence/uniqueness, name already active, and create-in-flight. The
// authoritative guards still live inside Create.
func (m *Manager) CreatePreflight(spec CreateSpec) error {
	if !isValidRepoName(spec.Name) {
		return ErrInvalidName
	}
	origin := ""
	if spec.Mode == "clone" {
		if spec.Origin == nil || spec.Origin.URL == "" {
			return fmt.Errorf("%w: clone mode requires origin.url", ErrInvalidName)
		}
		origin = spec.Origin.URL
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

	// Determine the origin to reserve (clone mode only) before reserving, so the
	// reservation covers the whole clone — including the network fetch — and a
	// second clone of the same origin is blocked for that entire window.
	var origin string
	if spec.Mode == "clone" {
		if spec.Origin == nil || spec.Origin.URL == "" {
			return nil, fmt.Errorf("%w: clone mode requires origin.url", ErrInvalidName)
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
	dbPath := filepath.Join(m.deps.Cfg.Home, "repos", spec.Name+".db")
	cleanup := func() {
		os.Remove(dbPath)
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
	}

	if cerr := ctx.Err(); cerr != nil {
		cleanup()
		return nil, cerr
	}

	switch spec.Mode {
	case "preset", "custom":
		if ierr := m.initLocal(ctx, spec, dbPath, emit); ierr != nil {
			cleanup()
			return nil, ierr
		}
	case "clone":
		// Name/origin presence and uniqueness were validated and reserved up
		// front via reserveNameAndOrigin; just clone.
		if ierr := m.initClone(ctx, spec, dbPath, emit); ierr != nil {
			cleanup()
			return nil, ierr
		}
	default:
		return nil, fmt.Errorf("%w: unknown mode %q", ErrInvalidName, spec.Mode)
	}

	if cerr := ctx.Err(); cerr != nil {
		cleanup()
		return nil, cerr
	}

	emit(Event{Step: "register", Message: "registering repo", Pct: 85})
	if aerr := m.Add(spec.Name, dbPath); aerr != nil {
		cleanup()
		return nil, fmt.Errorf("register repo: %w", aerr)
	}
	ri := m.Get(spec.Name)

	if spec.Mode == "clone" && ri != nil {
		emit(Event{Step: "sync", Message: "activating sync", Pct: 95})
		if serr := ri.ActivateSync(spec.Origin.URL); serr != nil {
			log.Warn().Err(serr).Str("repo", spec.Name).Msg("create: activate sync failed")
		}
	}

	// Write through to the registry: it, not the filesystem, is what Start reads
	// to decide this repo exists. A repo that is on disk but not in the registry
	// comes back from the dead as "missing" at the next boot.
	//
	// Logged rather than returned: at this point the repo is fully built,
	// registered and serving, so failing the call would report a create that
	// visibly succeeded — and a retry would then hit ErrRepoExists. The row is
	// re-derivable (Rescan re-registers it); a bogus error is not.
	if reg := m.RepoRegistry(); reg != nil {
		rec := RepoRecord{Name: spec.Name, State: RepoActive, CreatedAt: time.Now().UTC()}
		// Only clone mode actually attaches the origin to the store. Recording
		// spec.Origin for a preset/custom create would tell a later rebuild to
		// clone from a remote this repo was never connected to.
		if spec.Mode == "clone" && spec.Origin != nil {
			rec.OriginURL = spec.Origin.URL
			rec.OriginBranch = spec.Origin.Branch
		}
		if uerr := reg.Upsert(rec); uerr != nil {
			log.Error().Err(uerr).Str("repo", spec.Name).
				Msg("create: repo built but registry write failed; it will not survive a restart until rescanned")
		} else if rec.OriginURL != "" {
			// Reconcile against what the store ACTUALLY recorded. The row above
			// carries the branch the caller asked for, which is empty whenever
			// the caller let the remote decide — and initClone then resolved it
			// (detectUpstream) and wrote the real name into the store. Leaving
			// the row's blank in place would have a later rebuild clone with no
			// upstream pin on a master-default remote, which is the mismatch
			// detectUpstream exists to prevent in the first place.
			m.RecordOrigin(spec.Name)
		}
	}

	// Start replicating the new database NOW, not at the next restart.
	//
	// Boot-time tracking only covers what Start opened, so without this a repo
	// created through the API stays unreplicated for its entire first lifetime,
	// so the next cold boot has to rebuild it from origin instead of restoring
	// it — which is the whole cost the replica exists to avoid.
	//
	// Ordered after the registry write on purpose: the registry row is what
	// makes the next boot look for this database, and a database replicated
	// without a row would be restored by nothing.
	//
	// Logged rather than returned, for the same reason as the registry write
	// above: the repo is built, registered and serving, so failing the call
	// would report a create that visibly succeeded. The message says plainly
	// that the repo is unprotected, because a retry cannot fix it — Create
	// would hit ErrRepoExists — and a restart is the actual remedy.
	//
	// That remedy is only honest because the ONE failure a restart could not fix
	// has been removed upstream instead of handled here. The tracker refuses a
	// name already bound to a different file, and a repo named "control" hit
	// exactly that against <home>/control.db: logged here, the repo went on
	// serving unreplicated, and the next boot — where trackForReplication
	// RETURNS the same refusal — would not start at all. Reserving the name in
	// isValidRepoName makes the case unreachable from Create. It is fixed there
	// and not by returning the error here on purpose: the check that repos owns
	// is "is this name legal", and distinguishing the tracker's sentinels would
	// mean importing internal/backup, which repos must never do (see
	// BackupTracker). Every failure that can still reach this line is one a
	// restart genuinely does retry.
	if m.deps.Backup != nil {
		if terr := m.deps.Backup.Track(spec.Name, dbPath); terr != nil {
			log.Error().Err(terr).Str("repo", spec.Name).Str("db", dbPath).
				Msg("create: repo built but replication did not start; it is NOT backed up until the server is restarted")
		}
	}

	emit(Event{Step: "done", Message: "repo ready", Pct: 100})
	return ri, nil
}

// originOf reports a repo's configured origin remote (URL and upstream branch).
// It is the registry's window onto the store's own remote record, which stays
// the source of truth.
//
// ok separates the two ways this returns empty strings, and conflating them is
// a bug in both directions:
//
//   - ok=true with an empty url means the repo GENUINELY has no origin — it was
//     created without one, or the user disconnected it. The registry must be
//     brought into line, INCLUDING clearing an origin it still holds. Treating
//     this as "no information" is how a disconnected origin stays in control.db
//     and a later boot re-clones from a remote the user deliberately removed.
//   - ok=false means the store could not be read at all (detached handle, or a
//     failing query). Nothing was learned, so the registry's copy is the better
//     record and must be left exactly as it is. Treating this as "no origin" is
//     how a transient read failure erases the one field Manager.Start needs to
//     rebuild a repo whose database is gone.
//
// A remote row that is simply absent is err=nil, rm=nil from GetRemote — the
// first case, not the second.
func originOf(ri *RepoInstance) (url, branch string, ok bool) {
	if ri == nil {
		return "", "", false
	}
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		rm, err := svc.Remote().GetRemote("origin")
		if err != nil {
			return
		}
		ok = true
		if rm != nil {
			url = rm.URL
			branch = rm.Branch
		}
	})
	return url, branch, ok
}

// initLocal handles preset/custom modes: resolve ontology bytes, seed a fresh repo.
func (m *Manager) initLocal(ctx context.Context, spec CreateSpec, dbPath string, emit func(Event)) error {
	emit(Event{Step: "ontology", Message: "resolving ontology", Pct: 20})
	var ont *fact.Ontology
	var err error
	switch {
	case spec.Mode == "custom":
		ont, err = fact.ParseOntology([]byte(spec.OntologyYAML))
		if err != nil {
			return fmt.Errorf("parse ontology: %w", err)
		}
	case spec.OntologyPreset != "":
		ont, err = fact.OntologyByPreset(spec.OntologyPreset)
		if err != nil {
			return err
		}
	default:
		ont = fact.DefaultOntology()
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
		"domains/ontology.yaml": string(y),
	}, m.deps.AgentBranch); err != nil {
		return fmt.Errorf("init git: %w", err)
	}
	return nil
}

// initClone handles clone mode: fetch from origin, seed branches, persist remote.
func (m *Manager) initClone(ctx context.Context, spec CreateSpec, dbPath string, emit func(Event)) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	emit(Event{Step: "clone", Message: "cloning from " + spec.Origin.URL, Pct: 40})
	auth, err := m.ResolveAuth(authConfigFromSpec(spec.Origin), spec.Origin.URL)
	if err != nil {
		return fmt.Errorf("resolve auth: %w", err)
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
	// Without a Crypt, SetRemote refuses to persist the origin token (never
	// plaintext); configureCrypt logs a warning so that refusal is observable.
	configureCrypt(svc, m.deps.KeyPath, spec.Name)
	// Resolve the upstream BEFORE cloning, so the clone, the git refspecs and
	// the remotes row all name the same branch. Passing "" through to
	// InitFromRemote would let the store detect the branch while SetRemote below
	// still recorded "main", and the second configureRemote would then rewrite
	// the refspecs to a branch that does not exist on a master-default remote.
	upstream := spec.Origin.Branch
	if upstream == "" {
		upstream = detectUpstream(spec.Name, spec.Origin.URL, auth, m.deps.Cfg.Git.NetworkTimeout)
	}
	// Seed files are consumed ONLY when the origin turns out to be empty (there
	// is nothing to clone, so the repo is bootstrapped inline); a non-empty
	// origin supplies its own ontology and ignores these. Without them a repo
	// cloned from an empty remote comes up with no domains/ontology.yaml at all,
	// which the removed default-repo bootstrap used to prevent.
	seedFiles := map[string]string{}
	if ontologyYAML, serr := fact.DefaultOntology().Serialize(); serr != nil {
		log.Warn().Err(serr).Str("repo", spec.Name).
			Msg("clone: could not serialize the default ontology; an empty origin will seed without one")
	} else {
		seedFiles["domains/ontology.yaml"] = string(ontologyYAML)
	}
	if err := svc.InitFromRemote(spec.Origin.URL, auth, upstream, m.deps.AgentBranch, seedFiles); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	emit(Event{Step: "persist-origin", Message: "saving remote config", Pct: 70})
	if err := svc.Remote().SetRemote("origin", spec.Origin.URL, upstream, m.deps.AgentBranch, 300, 300, spec.Origin.AuthMethod, spec.Origin.AuthToken); err != nil {
		return fmt.Errorf("persist origin: %w", err)
	}
	return nil
}

// detectUpstream queries the origin's symbolic HEAD for the remote's default
// branch name (e.g. "main", "master"), falling back to "main" — but emitting a
// warn log first, so an operator can see that detection actually FAILED rather
// than the remote genuinely defaulting to main.
//
// Detection failure typically means: bad auth (token wrong/expired),
// unreachable URL (DNS/network), or the remote has no symbolic HEAD set. In all
// three cases the operator needs a signal — silently picking "main" for a
// master-default repo creates a configuration mismatch that the user only
// notices when origin/main forever appears empty.
func detectUpstream(repo, url string, auth transport.AuthMethod, timeout time.Duration) string {
	if upstream := store.DetectRemoteUpstreamFromURL(url, auth, timeout); upstream != "" {
		log.Info().Str("repo", repo).Str("upstream", upstream).
			Msg("clone: detected upstream branch from remote HEAD")
		return upstream
	}
	log.Warn().Str("repo", repo).Str("origin", url).
		Msg("clone: could not detect remote HEAD; defaulting to \"main\" (check auth/connectivity if origin uses a different default)")
	return "main"
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

// ActiveRepoWithOrigin returns the name of an active repo whose origin remote
// URL equals url, or "" if none. Enforces origin-uniqueness on clone/restore.
func (m *Manager) ActiveRepoWithOrigin(url string) string {
	var match string
	m.ForEach(func(name string, ri *RepoInstance) {
		if match != "" {
			return
		}
		ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				return
			}
			rm, err := svc.Remote().GetRemote("origin")
			if err == nil && rm != nil && rm.URL == url {
				match = name
			}
		})
	})
	return match
}

// ArchiveInfo describes one archived repo (manifest + derived id).
type ArchiveInfo struct {
	ID         string `json:"id"` // ksuid — globally unique, k-sortable by archive time
	Name       string `json:"name"`
	Origin     string `json:"origin"`
	ArchivedAt string `json:"archivedAt"`
}

func (m *Manager) archiveDir() string {
	return filepath.Join(m.deps.Cfg.Home, "repos", "archive")
}

// reinstateLive puts a repo back the way an aborted Archive found it: registered
// in the active map, and replicating again.
//
// The replication half is what makes this a helper rather than a bare m.Add.
// Archive now stops replication BEFORE it moves the file, so every recovery path
// after that point is undoing an untrack as well as a deregistration — and a
// repo that came back live but unreplicated would be silently unprotected until
// the next restart, which is the exact class of failure the backup work exists
// to remove. Best-effort and logged: the caller is already returning the error
// that caused the abort, and it must not be replaced by a recovery error.
func (m *Manager) reinstateLive(name, dbPath, stage string) {
	if aerr := m.Add(name, dbPath); aerr != nil {
		log.Error().Err(aerr).Str("repo", name).Str("stage", stage).
			Msg("archive: re-register failed; repo unregistered")
		return
	}
	// Re-derive the origin from the store now the repo is open again. It is a
	// no-op when the registry row already agrees, and it is what fills the row
	// back in on the one path that could not preserve it — a rollback whose read
	// of the prior active row failed, which re-registers with EnsureActive and
	// therefore with no origin at all. Leaving that blank would have a later
	// rebuild find nothing to clone from.
	m.RecordOrigin(name)
	if m.deps.Backup == nil {
		return
	}
	if terr := m.deps.Backup.Track(name, dbPath); terr != nil {
		log.Error().Err(terr).Str("repo", name).Str("db", dbPath).Str("stage", stage).
			Msg("archive: repo restored but replication did NOT restart; it is NOT backed up until the server is restarted")
	}
}

// Archive shuts down the named repo, moves its .db into the archive dir under a
// timestamped id, records the archive in the registry, and unregisters it.
//
// ANY repo can be archived, including the last one — zero repos is a valid
// state, and refusing to archive the only repo would leave a user who created
// one by mistake with no way to undo it. The guards that used to protect the
// default repo and the last repo are gone with the default repo itself.
func (m *Manager) Archive(name string) (ArchiveInfo, error) {
	// Reserve the name for the WHOLE operation, in the same in-flight set
	// Create/Restore/CreateLens use.
	//
	// Archive removes the repo from m.repos and only then shuts it down, makes
	// the archive dir, stops replication and moves the file — and m.Get(name)
	// returns nil for that entire window. Without the reservation a Create for
	// the same name lands inside it and every guard passes: its m.Get check
	// sees nothing, so initLocal opens repos/<name>.db — the ARCHIVED repo's
	// file, which has not been renamed yet — and if its trailing Track wins the
	// race to register, the Untrack below silently removes the BRAND-NEW live
	// repo's tracker before the rename carries its database into the archive
	// dir. A live repo with no database and no replication, and not one log line
	// about it.
	//
	// Reserved before m.mu is taken: reserveNameAndOrigin with an empty origin
	// touches only inflightMu (the active-origin scan that would take m.mu is
	// skipped), so there is no lock-order coupling here.
	release, err := m.reserveNameAndOrigin(name, "")
	if err != nil {
		return ArchiveInfo{}, err
	}
	defer release()

	// Verify the repo exists and remove it from the map under one Lock, so a
	// concurrent Archive of the same name cannot both proceed past the nil check.
	m.mu.Lock()
	ri := m.repos[name]
	if ri == nil {
		m.mu.Unlock()
		return ArchiveInfo{}, fmt.Errorf("%w: %q", ErrRepoNotFound, name)
	}
	// Direct field read is safe here: we hold m.mu (the write lock) already, so
	// the accessor's RLock would deadlock — read the field directly instead.
	if m.registry != nil {
		refs, rerr := m.registry.RefsRepo(name)
		if rerr != nil {
			m.mu.Unlock()
			return ArchiveInfo{}, fmt.Errorf("lens registry: %w", rerr)
		}
		if len(refs) > 0 {
			m.mu.Unlock()
			return ArchiveInfo{}, fmt.Errorf("%w: %q (lenses: %s)", ErrRepoInUseByLens, name, strings.Join(refs, ", "))
		}
	}
	delete(m.repos, name)
	m.mu.Unlock()

	// The ri pointer is still valid after the delete; capture origin before
	// anything is torn down. An unreadable store is recorded as no origin here,
	// unlike RecordOrigin's write-through: this row is being CREATED, so there
	// is no prior value to preserve, and the archived row's origin is a
	// best-effort note for a later Restore rather than a rebuild instruction.
	origin, originBranch, _ := originOf(ri)

	srcDB := filepath.Join(m.deps.Cfg.Home, "repos", name+".db")

	// Hand replication over in this order: stop the LIVE entry, tear the repo
	// down, move the file, start the ARCHIVE entry.
	//
	// Before the MOVE, because the tracker pins a file descriptor when it starts
	// replicating (litestream.DB.init opens the path exactly once, guarded so it
	// never reopens). Untracking after the move would leave a window in which the
	// live entry is still replicating the MOVED file — publishing archived
	// content under the live repo's prefix — and worse, the final sync that
	// untracking performs would write that moved file's state there as the live
	// repo's last word.
	//
	// Before the SHUTDOWN, because Untrack's final sync READS the database, and
	// ri.shutdown closes knomit's SQLite handle — which, without PERSIST_WAL,
	// checkpoints and DELETES the -wal. Untracking afterwards makes that sync
	// fail with "open <db>-wal: no such file or directory" and takes the whole
	// archive down with it. Pause documents the same ordering for the same
	// reason: litestream's connection must go before knomit's.
	//
	// The gap before TrackArchived is not a data gap. The untrack's final sync
	// has left a COMPLETE copy under the live prefix, and TrackArchived puts
	// another under the archive prefix; during the window the bytes are
	// duplicated, never absent.
	if m.deps.Backup != nil {
		if err := m.deps.Backup.Untrack(name); err != nil {
			// Nothing is torn down or moved yet and ri is still a live instance,
			// so put THAT instance back rather than opening a second handle on
			// the same database through Add, and restore its replication.
			m.Set(name, ri)
			if terr := m.deps.Backup.Track(name, srcDB); terr != nil {
				log.Error().Err(terr).Str("repo", name).Str("db", srcDB).
					Msg("archive: aborted, and replication did NOT restart; the repo is NOT backed up until the server is restarted")
			}
			return ArchiveInfo{}, fmt.Errorf("archive %q: stop replication: %w", name, err)
		}
	}

	ri.shutdown() // releases the SQLite file handle

	if err := os.MkdirAll(m.archiveDir(), 0o755); err != nil {
		// Recovery: re-register the repo so it is not lost.
		m.reinstateLive(name, srcDB, "mkdir")
		return ArchiveInfo{}, err
	}
	// A ksuid is globally unique, so archiving the same name twice within the
	// same second can never collide on the on-disk id (the old "<name>.<unix>"
	// scheme could). It is also k-sortable by creation time, which ListArchived
	// uses to order newest-first.
	now := time.Now().UTC()
	id := ksuid.New().String()
	dstDB := filepath.Join(m.archiveDir(), id+".db")

	if err := os.Rename(srcDB, dstDB); err != nil {
		// The db file is still at srcDB — re-register so the repo is not lost.
		m.reinstateLive(name, srcDB, "move-db")
		return ArchiveInfo{}, fmt.Errorf("move db: %w", err)
	}
	_ = os.Rename(srcDB+"-wal", dstDB+"-wal")
	_ = os.Rename(srcDB+"-shm", dstDB+"-shm")
	sess := filepath.Join(m.deps.Cfg.Home, "repos", name+store.SessionDBSuffix)
	os.Remove(sess)
	os.Remove(sess + "-wal")
	os.Remove(sess + "-shm")

	// Start replicating the archived copy BEFORE the registry is touched, so a
	// failure here is still fully reversible by the same move-back the rename
	// failure above uses. Ordered after the registry write instead, a failure
	// would have to report an error for an archive that had already happened.
	if m.deps.Backup != nil {
		if err := m.deps.Backup.TrackArchived(id, dstDB); err != nil {
			if rerr := os.Rename(dstDB, srcDB); rerr != nil {
				log.Error().Err(rerr).Str("repo", name).Str("id", id).
					Msg("archive: move db back after archive-replication failure failed")
			} else {
				_ = os.Rename(dstDB+"-wal", srcDB+"-wal")
				_ = os.Rename(dstDB+"-shm", srcDB+"-shm")
				m.reinstateLive(name, srcDB, "replicate-archive")
			}
			return ArchiveInfo{}, fmt.Errorf("archive %q: replicate archived copy: %w", name, err)
		}
	}

	info := ArchiveInfo{
		ID:         id,
		Name:       name,
		Origin:     origin,
		ArchivedAt: now.Format(time.RFC3339Nano),
	}
	// The registry row REPLACES repos/archive/<id>.json: litestream replicates
	// SQLite only, so a manifest sitting next to the db could never travel with
	// a backup. Same failure handling the manifest write had — the archive
	// record is what makes the moved db findable again, so losing it would
	// strand the repo.
	if reg := m.RepoRegistry(); reg != nil {
		// Capture the row we are about to retire so a failure can put it back.
		// A read failure is not fatal: rollback re-registers the repo from
		// scratch instead, losing only the provenance this read would have
		// carried — and RecordOrigin puts most of that back from the store.
		prior, hadPrior, perr := reg.ActiveRecord(name)
		if perr != nil {
			log.Warn().Err(perr).Str("repo", name).
				Msg("archive: could not read the active registry row; a rollback will re-register the repo " +
					"without its recorded creation time")
		}
		// Undo everything this function has done, in reverse: the archived row,
		// the active row, the db move, the registration. Best-effort — each step
		// logs and continues so one failure cannot skip the rest.
		rollback := func(stage string) {
			if derr := reg.DeleteArchive(id); derr != nil {
				log.Error().Err(derr).Str("repo", name).Str("id", id).Str("stage", stage).
					Msg("archive: rollback could not remove the archived row")
			}
			// The active row has to come back, and when the read above failed
			// there is no row to put back — so register the repo afresh rather
			// than leaving it live-but-unregistered. That state is invisible to
			// the next Start (the registry is authoritative and the disk is no
			// longer consulted), so the repo would simply not come back after a
			// restart, with nothing in the log saying so.
			//
			// EnsureActive, not Upsert: it touches state and archived_at only, so
			// a row that does exist keeps whatever provenance it holds.
			var uerr error
			if hadPrior {
				uerr = reg.Upsert(prior)
			} else {
				uerr = reg.EnsureActive(name, time.Now().UTC())
			}
			if uerr != nil {
				log.Error().Err(uerr).Str("repo", name).Str("stage", stage).
					Msg("archive: rollback could not restore the active row; repo will not survive a restart until rescanned")
			}
			// Before the file moves back, for the same pinned-descriptor reason
			// the forward direction untracks before its move.
			if m.deps.Backup != nil {
				if uerr := m.deps.Backup.UntrackArchived(id); uerr != nil {
					log.Error().Err(uerr).Str("repo", name).Str("id", id).Str("stage", stage).
						Msg("archive: rollback could not stop replicating the archived copy")
				}
			}
			if rerr := os.Rename(dstDB, srcDB); rerr != nil {
				log.Error().Err(rerr).Str("repo", name).Str("stage", stage).Msg("archive: move db back failed")
				return
			}
			_ = os.Rename(dstDB+"-wal", srcDB+"-wal")
			_ = os.Rename(dstDB+"-shm", srcDB+"-shm")
			m.reinstateLive(name, srcDB, stage)
		}

		rec := RepoRecord{
			Name:         name,
			OriginURL:    origin,
			OriginBranch: originBranch,
			State:        RepoArchived,
			ArchiveID:    id,
			ArchivedAt:   now,
			CreatedAt:    prior.CreatedAt,
		}
		if err := reg.Upsert(rec); err != nil {
			rollback("record-archive")
			return ArchiveInfo{}, fmt.Errorf("record archive: %w", err)
		}
		// Retire the ACTIVE row. Under the composite key the archived row above
		// is a NEW row, so without this the repo stays registered as active with
		// no database behind it — which the next Start would either re-clone
		// (resurrecting an archived repo). Deliberately AFTER the archived row lands: the window where
		// both rows exist is recoverable, the window where neither does is not.
		if err := reg.DeleteActive(name); err != nil {
			rollback("retire-active")
			return ArchiveInfo{}, fmt.Errorf("retire active registration: %w", err)
		}
	}
	log.Info().Str("repo", name).Str("id", id).Msg("archived repo")
	return info, nil
}

// ListArchived returns every archived repo the registry knows about, newest
// first. The source used to be repos/archive/*.json; the ordering contract and
// the ArchiveInfo shape callers see are unchanged.
func (m *Manager) ListArchived() ([]ArchiveInfo, error) {
	reg := m.RepoRegistry()
	if reg == nil {
		return []ArchiveInfo{}, nil // before Start there is nothing to list
	}
	rows, err := reg.List(RepoArchived)
	if err != nil {
		return nil, err
	}
	out := make([]ArchiveInfo, 0, len(rows))
	for _, rec := range rows {
		info := ArchiveInfo{
			ID:     rec.ArchiveID,
			Name:   rec.Name,
			Origin: rec.OriginURL,
		}
		if !rec.ArchivedAt.IsZero() {
			info.ArchivedAt = rec.ArchivedAt.Format(time.RFC3339Nano)
		}
		out = append(out, info)
	}
	// Order by archive time, newest first. ArchivedAt is the authoritative
	// recency signal; the ksuid id is a stable tiebreak (and the fallback when
	// a legacy manifest has an unparseable timestamp).
	sort.Slice(out, func(i, j int) bool {
		ti, ei := time.Parse(time.RFC3339Nano, out[i].ArchivedAt)
		tj, ej := time.Parse(time.RFC3339Nano, out[j].ArchivedAt)
		if ei == nil && ej == nil && !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// findArchived returns the manifest for archiveID or ErrArchiveNotFound.
func (m *Manager) findArchived(archiveID string) (ArchiveInfo, error) {
	all, err := m.ListArchived()
	if err != nil {
		return ArchiveInfo{}, err
	}
	for _, a := range all {
		if a.ID == archiveID {
			return a, nil
		}
	}
	return ArchiveInfo{}, fmt.Errorf("%w: %q", ErrArchiveNotFound, archiveID)
}

// fetchArchivedDB pulls an archived repo's database back from the replica when
// it is not on this volume, and is the second half of the archive round trip —
// without it the first half is decoration.
//
// Bootstrap restores ACTIVE repos only. control.db comes back with every
// archived row inside it, so ListArchived still advertises this repo as
// restorable, but repos/archive/<id>.db does not exist on a replaced container.
// Restore would then reach os.Rename and fail with a bare "no such file or
// directory" that names no cause, while the copy Archive deliberately wrote
// under a never-expiring prefix sat in the bucket unused.
//
// Every failure here names the actual cause, because this is the error an
// operator meets when an unarchive does not work.
func (m *Manager) fetchArchivedDB(archiveID, dstDB string) error {
	if m.deps.Backup == nil {
		return fmt.Errorf("%w: %q (its database is not on this volume, and backup is disabled so there is nowhere to fetch it from)",
			ErrArchiveNotFound, archiveID)
	}
	if err := os.MkdirAll(filepath.Dir(dstDB), 0o755); err != nil {
		return fmt.Errorf("restore %q: create archive dir: %w", archiveID, err)
	}
	restored, err := m.deps.Backup.RestoreArchived(archiveID, dstDB)
	if err != nil {
		return fmt.Errorf("restore %q: its database is not on this volume and could not be fetched from the replica: %w", archiveID, err)
	}
	if !restored {
		return fmt.Errorf("%w: %q (its database is not on this volume and the replica holds no backup for it)",
			ErrArchiveNotFound, archiveID)
	}
	log.Info().Str("id", archiveID).Str("db", dstDB).
		Msg("restore: archived database was not on this volume; fetched it from the replica")
	return nil
}

// reinstateArchived puts an archived repo's replication back after an aborted
// Restore, so a repo that stays archived also stays backed up. It is Restore's
// counterpart to reinstateLive, and is best-effort and logged for the same
// reason: the caller is already returning the error that caused the abort.
func (m *Manager) reinstateArchived(archiveID, dbPath, stage string) {
	if m.deps.Backup == nil {
		return
	}
	// Only when the database is actually there. TrackArchived against a missing
	// path SUCCEEDS — litestream opens the file lazily, so registration does not
	// notice — and leaves behind a phantom entry that can never sync and logs an
	// error on every monitor tick for the life of the process. An unarchive that
	// failed to put the file back must not also install a permanent liar in the
	// tracked set.
	if _, err := os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			log.Warn().Err(err).Str("id", archiveID).Str("db", dbPath).Str("stage", stage).
				Msg("restore: could not stat the archived database while reinstating its replication")
		}
		return
	}
	if terr := m.deps.Backup.TrackArchived(archiveID, dbPath); terr != nil {
		log.Error().Err(terr).Str("id", archiveID).Str("db", dbPath).Str("stage", stage).
			Msg("restore: aborted, and replication of the archived copy did NOT restart; it is NOT backed up until the server is restarted")
	}
}

// Restore re-activates an archived repo, optionally under newName to resolve a
// name collision. Fails if the target name is active, or if the archived repo's
// origin matches an active repo's origin.
func (m *Manager) Restore(archiveID, newName string) (*RepoInstance, error) {
	info, err := m.findArchived(archiveID)
	if err != nil {
		return nil, err
	}
	target := info.Name
	if newName != "" {
		target = newName
	}
	if !isValidRepoName(target) {
		return nil, ErrInvalidName
	}

	// Reserve the target name and the archived origin for the whole check → Add
	// window so a concurrent Create/Restore can't race us to register the same
	// name (clobbering each other's .db) or attach the same origin to two repos.
	// reserveNameAndOrigin also performs the authoritative active-origin scan.
	release, err := m.reserveNameAndOrigin(target, info.Origin)
	if err != nil {
		return nil, err
	}
	defer release()

	if m.Get(target) != nil {
		return nil, fmt.Errorf("%w: %q", ErrRepoExists, target)
	}
	// Reverse M-1 guard: an archived repo's name can be claimed by a lens while
	// the repo sits archived (ValidateLens's active-only m.Get lets it through),
	// so restoring back to active must refuse a name a lens now holds.
	if err := m.lensNameConflict(target); err != nil {
		return nil, err
	}

	srcDB := filepath.Join(m.archiveDir(), archiveID+".db")
	dstDB := filepath.Join(m.deps.Cfg.Home, "repos", target+".db")

	// Guard against a leftover destination file from a prior failed restore.
	// Renaming over it would clobber an unrelated db; refuse instead.
	if _, err := os.Stat(dstDB); err == nil {
		return nil, fmt.Errorf("%w: %q (db file already exists)", ErrRepoExists, target)
	}

	// The archived database may not be on this volume at all — see
	// fetchArchivedDB. Done BEFORE the untrack below because a database that had
	// to be fetched was, by definition, not being replicated from here.
	if _, serr := os.Stat(srcDB); os.IsNotExist(serr) {
		if ferr := m.fetchArchivedDB(archiveID, srcDB); ferr != nil {
			return nil, ferr
		}
	} else if serr != nil {
		return nil, fmt.Errorf("restore %q: stat archived database: %w", target, serr)
	}

	// Reverse Archive's handover, in the mirror-image order: stop the ARCHIVE
	// entry, move the file, start the LIVE one. Untracking before the move is
	// required for the same reason it is there — the tracker pins a file
	// descriptor, so an archive entry left running would keep replicating a file
	// that has moved back to the live path, publishing live writes under the
	// archive prefix. Nothing has moved yet at this point, so a failure here is
	// a clean refusal.
	if m.deps.Backup != nil {
		if err := m.deps.Backup.UntrackArchived(archiveID); err != nil {
			return nil, fmt.Errorf("restore %q: stop replicating the archived copy: %w", target, err)
		}
	}

	if err := os.Rename(srcDB, dstDB); err != nil {
		m.reinstateArchived(archiveID, srcDB, "move-db")
		return nil, fmt.Errorf("restore move: %w", err)
	}
	_ = os.Rename(srcDB+"-wal", dstDB+"-wal")
	_ = os.Rename(srcDB+"-shm", dstDB+"-shm")

	if err := m.Add(target, dstDB); err != nil {
		// Recovery: move the db back to the archive path so the repo remains a
		// recoverable archived entry. Do NOT drop the archive's registry row.
		if rerr := os.Rename(dstDB, srcDB); rerr != nil {
			log.Error().Err(rerr).Str("repo", target).Msg("restore: move db back after register failure failed")
		} else {
			_ = os.Rename(dstDB+"-wal", srcDB+"-wal")
			_ = os.Rename(dstDB+"-shm", srcDB+"-shm")
			m.reinstateArchived(archiveID, srcDB, "register")
		}
		return nil, fmt.Errorf("restore register: %w", err)
	}

	ri := m.Get(target)

	// Replicate under the live name from here on. Logged rather than returned,
	// for the same reason Create logs its Track failure: the repo is registered
	// and serving by now, so failing the call would report a restore that
	// visibly succeeded, and the retry would hit ErrRepoExists.
	if m.deps.Backup != nil {
		if terr := m.deps.Backup.Track(target, dstDB); terr != nil {
			log.Error().Err(terr).Str("repo", target).Str("db", dstDB).
				Msg("restore: repo is live but replication did not start; it is NOT backed up until the server is restarted")
		}
	}

	// Only now that the repo is registered is it safe to retire the archive
	// row. Add the active row FIRST: if the process dies between the two, a
	// duplicate registration is recoverable, whereas a repo with no row at all
	// is invisible to the next Start. Restore can rename, so this is a new row
	// under `target`, not a state flip on the archived one.
	if reg := m.RepoRegistry(); reg != nil {
		originURL, originBranch, _ := originOf(ri)
		if originURL == "" {
			originURL = info.Origin
		}
		// Carry the repo's original creation time across the archive/restore
		// round trip. Stamping time.Now() here would make every restored repo
		// look newly created, quietly rewriting its provenance — the same reason
		// rebuildFromOrigin writes the pre-existing record back after Create.
		created := time.Now().UTC()
		if arch, ok, aerr := reg.ArchiveRecord(archiveID); aerr != nil {
			log.Warn().Err(aerr).Str("id", archiveID).
				Msg("restore: could not read the archived row; creation time will be reset")
		} else if ok && !arch.CreatedAt.IsZero() {
			created = arch.CreatedAt
			if originBranch == "" {
				originBranch = arch.OriginBranch
			}
		}
		rec := RepoRecord{
			Name:         target,
			OriginURL:    originURL,
			OriginBranch: originBranch,
			State:        RepoActive,
			CreatedAt:    created,
		}
		if uerr := reg.Upsert(rec); uerr != nil {
			log.Error().Err(uerr).Str("repo", target).
				Msg("restore: repo is live but registry write failed; it will not survive a restart until rescanned")
		} else if derr := reg.DeleteArchive(archiveID); derr != nil {
			log.Error().Err(derr).Str("id", archiveID).
				Msg("restore: stale archive row left behind; it points at a db that has moved")
		}
	}
	removeLegacyManifest(m.archiveDir(), archiveID)

	if ri != nil && info.Origin != "" {
		if serr := ri.ActivateSync(info.Origin); serr != nil {
			log.Warn().Err(serr).Str("repo", target).Msg("restore: activate sync failed")
		}
	}
	log.Info().Str("id", archiveID).Str("repo", target).Msg("restored repo")
	return ri, nil
}

// Purge permanently deletes an archived repo's db and registry row.
func (m *Manager) Purge(archiveID string) error {
	info, err := m.findArchived(archiveID)
	if err != nil {
		return err
	}
	if reg := m.Registry(); reg != nil {
		refs, rerr := reg.RefsRepo(info.Name)
		if rerr != nil {
			return fmt.Errorf("lens registry: %w", rerr)
		}
		if len(refs) > 0 {
			return fmt.Errorf("%w: %q (lenses: %s)", ErrRepoInUseByLens, info.Name, strings.Join(refs, ", "))
		}
	}
	// Stop replicating before the file goes away. Purge is the one permanent
	// deletion in the lifecycle, so this is where a tracked entry for this repo
	// must not outlive it.
	//
	// The entry that actually belongs to this archive is the ARCHIVE one, keyed
	// by the archive id — a ksuid, which no repo can ever hold as a name — so it
	// needs no guard at all. That is the entry Archive created, and the one a
	// purge is unambiguously entitled to remove.
	if m.deps.Backup != nil {
		if uerr := m.deps.Backup.UntrackArchived(archiveID); uerr != nil {
			log.Warn().Err(uerr).Str("repo", info.Name).Str("id", archiveID).
				Msg("purge: could not stop replication for the purged archive")
		}
		m.untrackReclaimableName(info.Name, archiveID)

		// Then delete the replica objects, and FAIL the purge if that does not
		// work. This is not the usual best-effort backup cleanup: the archive
		// namespace runs with retention disabled, so nothing else will ever
		// reclaim these objects. Swallowing the error would quietly redefine
		// "purge" as "delete locally, keep forever in the bucket" — the opposite
		// of what the caller asked for, and unbounded storage growth.
		//
		// Ordered before the local delete and the registry row so a failure
		// leaves the archive fully intact and the purge simply retryable, rather
		// than half-done with the row that names the objects already gone.
		if derr := m.deps.Backup.DeleteArchivedReplica(archiveID); derr != nil {
			return fmt.Errorf("purge %q: the archived database is still in the replica and nothing else will ever remove it "+
				"(the archive namespace has retention disabled); the archive is unchanged, so this purge can be retried: %w",
				archiveID, derr)
		}
	}

	db := filepath.Join(m.archiveDir(), archiveID+".db")
	if err := os.Remove(db); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("purge db: %w", err)
	}
	os.Remove(db + "-wal")
	os.Remove(db + "-shm")
	// Drop the row by archive id, not by name: the archived repo's name may
	// since have been claimed by a live active repo, and Delete(name) would
	// take that one out too.
	if reg := m.RepoRegistry(); reg != nil {
		if derr := reg.DeleteArchive(archiveID); derr != nil {
			return fmt.Errorf("purge registry row: %w", derr)
		}
	}
	removeLegacyManifest(m.archiveDir(), archiveID)
	log.Info().Str("id", archiveID).Msg("purged repo")
	return nil
}

// untrackReclaimableName is Purge's defence-in-depth sweep of a tracked entry
// still keyed by the archived repo's NAME, and the only interesting thing about
// it is that the check and the act must be atomic.
//
// Why it exists at all: Archive stops replicating the live name before it moves
// the file, so in the ordinary course nothing is tracked under that name by the
// time a purge runs and this is a no-op. It stays as a sweep for entries an
// earlier, abandoned or partially-failed lifecycle could have left behind.
//
// Why it is guarded: an archived repo's name can be reclaimed by a NEW active
// repo — which is why the registry row below is dropped by archive id rather
// than by name — and that repo's replication is not a purge's to stop.
//
// Why the guard is a RESERVATION and not `m.Get(name) == nil`: those are two
// unsynchronised steps with nothing holding the name in between. A Create
// completing in that window registers and tracks a brand-new LIVE repo, and the
// purge then untracks it — leaving live data replicated by nothing, with Untrack
// returning cleanly so no log line anywhere records it. Taking the same
// reservation Create and Restore hold across their own check → Add → Track
// window makes the pair atomic against them: either we hold it and no create can
// land, or a create holds it and we skip — which is correct, because an entry
// under a name a create owns is that create's, not ours.
func (m *Manager) untrackReclaimableName(name, archiveID string) {
	release, err := m.reserveNameAndOrigin(name, "")
	if err != nil {
		log.Info().Str("repo", name).Str("id", archiveID).
			Msg("purge: an operation is bringing this name back to life; leaving its replication alone")
		return
	}
	defer release()
	if m.Get(name) != nil {
		return
	}
	if uerr := m.deps.Backup.Untrack(name); uerr != nil {
		log.Warn().Err(uerr).Str("repo", name).Str("id", archiveID).
			Msg("purge: could not stop replication for the purged repo")
	}
}

// removeLegacyManifest deletes a pre-registry repos/archive/<id>.json, if one
// is still lying around from before adoption. Nothing writes these any more and
// nothing reads them after the one-time adopt, but leaving a manifest for an
// archive that has since been restored or purged means a later adoption (on a
// machine that lost control.db) would resurrect a dead entry pointing at a db
// that is gone. Best-effort: a stale file is untidy, not fatal.
func removeLegacyManifest(archiveDir, archiveID string) {
	if err := os.Remove(filepath.Join(archiveDir, archiveID+".json")); err != nil && !os.IsNotExist(err) {
		log.Warn().Err(err).Str("id", archiveID).Msg("could not remove legacy archive manifest")
	}
}
