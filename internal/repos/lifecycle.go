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
	// ErrCannotArchiveDefault is returned when archiving the default repo.
	ErrCannotArchiveDefault = errors.New("cannot archive the default repo")
	// ErrCannotArchiveLast is returned when archiving the only active repo.
	ErrCannotArchiveLast = errors.New("cannot archive the last active repo")
	// ErrArchiveNotFound is returned when no archived repo matches.
	ErrArchiveNotFound = errors.New("archived repo not found")
	// ErrRepoNotFound is returned when no active repo matches a name.
	ErrRepoNotFound = errors.New("repo not found")
)

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
	release, err := m.reserveNameAndOrigin(spec.Name, origin)
	if err != nil {
		return nil, err
	}
	defer release()
	if m.Get(spec.Name) != nil { // re-check after reserving
		return nil, ErrRepoExists
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
	if keyData, rerr := os.ReadFile(m.deps.KeyPath); rerr == nil {
		if crypt, cerr := store.NewCrypt(keyData); cerr == nil {
			svc.SetCrypt(crypt)
		}
	}
	if err := svc.InitFromRemote(spec.Origin.URL, auth, spec.Origin.Branch, m.deps.AgentBranch, map[string]string{}); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	emit(Event{Step: "persist-origin", Message: "saving remote config", Pct: 70})
	upstream := spec.Origin.Branch
	if upstream == "" {
		upstream = "main"
	}
	if err := svc.Remote().SetRemote("origin", spec.Origin.URL, upstream, m.deps.AgentBranch, 300, 300, spec.Origin.AuthMethod, spec.Origin.AuthToken); err != nil {
		return fmt.Errorf("persist origin: %w", err)
	}
	return nil
}

// authConfigFromSpec maps an OriginSpec to the config shape ResolveAuth expects.
func authConfigFromSpec(o *OriginSpec) config.RemoteAuthConfig {
	return config.RemoteAuthConfig{
		Token:      o.AuthToken,
		Password:   o.AuthToken,
		AuthMethod: o.AuthMethod,
	}
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

// Archive shuts down the named repo, moves its .db into the archive dir under a
// timestamped id, writes a manifest, and unregisters it. The default repo and
// the last remaining active repo cannot be archived.
func (m *Manager) Archive(name string) (ArchiveInfo, error) {
	if name == config.DefaultRepoName {
		return ArchiveInfo{}, ErrCannotArchiveDefault
	}

	// Atomically: verify the repo exists, enforce the last-repo guard, and
	// remove it from the map. Doing all three under one Lock closes the TOCTOU
	// window where a concurrent Archive could see len>1, both delete, and leave
	// zero repos. ErrCannotArchiveLast is a defensive guard: in normal
	// operation the default repo (trunk) is always present and is rejected by
	// the default-repo check above, so this branch is only reachable if the
	// map has been reduced to a single non-default repo by other means.
	m.mu.Lock()
	ri := m.repos[name]
	if ri == nil {
		m.mu.Unlock()
		return ArchiveInfo{}, fmt.Errorf("%w: %q", ErrRepoNotFound, name)
	}
	if len(m.repos) <= 1 {
		m.mu.Unlock()
		return ArchiveInfo{}, ErrCannotArchiveLast
	}
	delete(m.repos, name)
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

	srcDB := filepath.Join(m.deps.Cfg.Home, "repos", name+".db")

	if err := os.MkdirAll(m.archiveDir(), 0o755); err != nil {
		// Recovery: re-register the repo so it is not lost.
		if aerr := m.Add(name, srcDB); aerr != nil {
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
		if aerr := m.Add(name, srcDB); aerr != nil {
			log.Error().Err(aerr).Str("repo", name).Msg("archive: re-register after rename failure failed; repo unregistered")
		}
		return ArchiveInfo{}, fmt.Errorf("move db: %w", err)
	}
	_ = os.Rename(srcDB+"-wal", dstDB+"-wal")
	_ = os.Rename(srcDB+"-shm", dstDB+"-shm")
	sess := filepath.Join(m.deps.Cfg.Home, "repos", name+store.SessionDBSuffix)
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
			if aerr := m.Add(name, srcDB); aerr != nil {
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

	if err := m.Add(target, dstDB); err != nil {
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
	if _, err := m.findArchived(archiveID); err != nil {
		return err
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
