package repos

import (
	"context"
	"encoding/json"
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
		if err := rejectOntologySpecForClone(spec); err != nil {
			return err
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
	if ierr := m.reg.Insert(rec); ierr != nil {
		return nil, ierr // ErrRepoExists when an active repo already holds the name
	}
	dbPath := m.RepoPath(uid)
	cleanup := func() {
		os.Remove(dbPath)
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
		if derr := m.reg.Delete(uid); derr != nil {
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
	default:
		cleanup()
		return nil, fmt.Errorf("%w: unknown mode %q", ErrInvalidName, spec.Mode)
	}

	if cerr := ctx.Err(); cerr != nil {
		cleanup()
		return nil, cerr
	}

	var originRec *Origin
	if spec.Mode == "clone" {
		emit(Event{Step: "persist-origin", Message: "saving remote config", Pct: 70})
		// The upstream InitFromRemote RESOLVED, never the one requested — see
		// initClone, which returns it for exactly this reason.
		originRec = &Origin{
			URL:        spec.Origin.URL,
			Branch:     resolvedUpstream,
			AuthMethod: spec.Origin.AuthMethod,
			AuthToken:  spec.Origin.AuthToken,
		}
		if oerr := m.origins.Set(uid, *originRec); oerr != nil {
			cleanup()
			return nil, fmt.Errorf("persist origin: %w", oerr)
		}
	}

	emit(Event{Step: "register", Message: "registering repo", Pct: 85})

	if aerr := m.Add(spec.Name, uid, dbPath, originRec); aerr != nil {
		cleanup()
		return nil, fmt.Errorf("register repo: %w", aerr)
	}
	ri := m.Get(spec.Name)
	if ri != nil {
		if id := ri.ID(); id != "" {
			if rerr := m.reg.RecordRepoID(uid, id); rerr != nil {
				m.Remove(spec.Name)
				cleanup()
				return nil, rerr // ErrRepoAlreadyRegistered for a mirror clone
			}
		}
	}

	if spec.Mode == "clone" && ri != nil {
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
		OntologyPath: string(y),
	}, m.deps.AgentBranch); err != nil {
		return fmt.Errorf("init git: %w", err)
	}
	return nil
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

// initClone handles clone mode: fetch from origin and seed branches. It does
// NOT persist the origin anywhere — Create does that, into control.db, once
// initClone returns.
//
// The returned upstream is the branch the clone ACTUALLY adopted, resolved by
// svc.InitFromRemote against the remote (prefer "main", else its symbolic
// HEAD) whenever spec.Origin.Branch is empty. Create MUST persist exactly this
// value, never the requested spec.Origin.Branch and never a defaulted "main":
// this repo's local branch and fetch refspecs were built from the RESOLVED
// branch, so persisting anything else writes an origin that disagrees with
// them, and every later sync reads a nonexistent origin/<branch>.
func (m *Manager) initClone(ctx context.Context, spec CreateSpec, dbPath string, emit func(Event)) (string, error) {
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	// The authoritative copy of the CreatePreflight check: Create is also called
	// directly (tests, future CLI paths) and the seed below would otherwise
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
	// Without a Crypt, credential storage is unavailable; configureCrypt logs a
	// warning so that is observable. (Task 17 removes this call along with the
	// store's own credential path.)
	configureCrypt(svc, m.deps.KeyPath, spec.Name)
	// Seed the ontology for the EMPTY-remote case. InitFromRemote ignores these
	// files when the remote has branches (their content comes from the clone),
	// and writes them onto the new agent branch when it does not — so without
	// them, cloning an empty origin yields a repo with no ontology file at all,
	// unlike every repo created through initLocal. The DEFAULT ontology is the
	// unambiguous choice here because a clone request may not name one (see
	// rejectOntologySpecForClone).
	ont, err := fact.DefaultOntology().Serialize()
	if err != nil {
		return "", fmt.Errorf("serialize ontology: %w", err)
	}
	upstream, err := svc.InitFromRemote(spec.Origin.URL, auth, spec.Origin.Branch, m.deps.AgentBranch,
		map[string]string{OntologyPath: string(ont)})
	if err != nil {
		return "", fmt.Errorf("clone: %w", err)
	}
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	return upstream, nil
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
	if m.origins == nil {
		return ""
	}
	name, err := m.origins.ActiveRepoWithURL(url)
	if err != nil {
		log.Warn().Err(err).Msg("origin uniqueness check failed; allowing the operation to proceed to the identity check")
		return ""
	}
	return name
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

// Archive shuts down the named repo, moves its .db into the archive dir under a
// timestamped id, writes a manifest, and unregisters it.
//
// ANY repo may be archived, including the last one: no repo is privileged, and
// zero repos is a valid state (it is how knomit starts). The archive is
// recoverable via Restore, so emptying the manager loses nothing.
func (m *Manager) Archive(name string) (ArchiveInfo, error) {
	// Verify the repo exists and remove it from the map under one Lock, so a
	// concurrent Archive of the same name cannot both observe it and both
	// proceed to move the file.
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
	uid := ri.uid
	if uid != "" {
		delete(m.byUID, uid)
	}
	m.mu.Unlock()

	// The ri pointer is still valid after the delete; capture origin then tear
	// it down so the SQLite handle is released before we move the file.
	var origin string
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		if rm, err := svc.Remote().GetRemote("origin"); err == nil && rm != nil {
			origin = rm.URL
		}
	})
	ri.shutdown() // releases the SQLite file handle

	// The file lives at <uid>.db (Manager.RepoPath), never at <name>.db — the
	// name is registry metadata only. Archive/Restore/Purge still move this
	// file by hand rather than flipping a registry state (Task 8 replaces this
	// with a pure control.db UPDATE that never touches the filesystem); until
	// then this is the bridging fix that keeps the move working under uid
	// paths.
	srcDB := m.RepoPath(uid)

	if err := os.MkdirAll(m.archiveDir(), 0o755); err != nil {
		// Recovery: re-register the repo so it is not lost.
		if aerr := m.Add(name, uid, srcDB, nil); aerr != nil {
			log.Error().Err(aerr).Str("repo", name).Msg("archive: re-register after mkdir failure failed; repo unregistered")
		}
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
		if aerr := m.Add(name, uid, srcDB, nil); aerr != nil {
			log.Error().Err(aerr).Str("repo", name).Msg("archive: re-register after rename failure failed; repo unregistered")
		}
		return ArchiveInfo{}, fmt.Errorf("move db: %w", err)
	}
	_ = os.Rename(srcDB+"-wal", dstDB+"-wal")
	_ = os.Rename(srcDB+"-shm", dstDB+"-shm")
	sess := strings.TrimSuffix(srcDB, ".db") + store.SessionDBSuffix
	os.Remove(sess)
	os.Remove(sess + "-wal")
	os.Remove(sess + "-shm")

	info := ArchiveInfo{
		ID:         id,
		Name:       name,
		Origin:     origin,
		ArchivedAt: now.Format(time.RFC3339Nano),
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(filepath.Join(m.archiveDir(), id+".json"), data, 0o644); err != nil {
		// Move the db back and re-register so the repo is recoverable as active.
		if rerr := os.Rename(dstDB, srcDB); rerr != nil {
			log.Error().Err(rerr).Str("repo", name).Msg("archive: move db back after manifest failure failed")
		} else {
			_ = os.Rename(dstDB+"-wal", srcDB+"-wal")
			_ = os.Rename(dstDB+"-shm", srcDB+"-shm")
			if aerr := m.Add(name, uid, srcDB, nil); aerr != nil {
				log.Error().Err(aerr).Str("repo", name).Msg("archive: re-register after manifest failure failed; repo unregistered")
			}
		}
		return ArchiveInfo{}, fmt.Errorf("write manifest: %w", err)
	}
	log.Info().Str("repo", name).Str("id", id).Msg("archived repo")
	return info, nil
}

// ListArchived reads all manifests under the archive dir, newest first.
func (m *Manager) ListArchived() ([]ArchiveInfo, error) {
	entries, err := os.ReadDir(m.archiveDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []ArchiveInfo{}, nil
		}
		return nil, err
	}
	out := []ArchiveInfo{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(m.archiveDir(), e.Name()))
		if rerr != nil {
			continue
		}
		var info ArchiveInfo
		if json.Unmarshal(data, &info) == nil {
			out = append(out, info)
		}
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

	if err := os.Rename(srcDB, dstDB); err != nil {
		return nil, fmt.Errorf("restore move: %w", err)
	}
	_ = os.Rename(srcDB+"-wal", dstDB+"-wal")
	_ = os.Rename(srcDB+"-shm", dstDB+"-shm")

	if err := m.Add(target, "", dstDB, nil); err != nil {
		// Recovery: move the db back to the archive path so the repo remains a
		// recoverable archived entry. Do NOT delete the manifest.
		if rerr := os.Rename(dstDB, srcDB); rerr != nil {
			log.Error().Err(rerr).Str("repo", target).Msg("restore: move db back after register failure failed")
		} else {
			_ = os.Rename(dstDB+"-wal", srcDB+"-wal")
			_ = os.Rename(dstDB+"-shm", srcDB+"-shm")
		}
		return nil, fmt.Errorf("restore register: %w", err)
	}

	// Only now that the repo is registered is it safe to drop the manifest.
	os.Remove(filepath.Join(m.archiveDir(), archiveID+".json"))

	ri := m.Get(target)
	if ri != nil && info.Origin != "" {
		if serr := ri.ActivateSync(info.Origin); serr != nil {
			log.Warn().Err(serr).Str("repo", target).Msg("restore: activate sync failed")
		}
	}
	log.Info().Str("id", archiveID).Str("repo", target).Msg("restored repo")
	return ri, nil
}

// Purge permanently deletes an archived repo's db and manifest.
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
	db := filepath.Join(m.archiveDir(), archiveID+".db")
	if err := os.Remove(db); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("purge db: %w", err)
	}
	os.Remove(db + "-wal")
	os.Remove(db + "-shm")
	os.Remove(filepath.Join(m.archiveDir(), archiveID+".json"))
	log.Info().Str("id", archiveID).Msg("purged repo")
	return nil
}
