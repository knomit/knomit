package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

var (
	// ErrRepoExists is returned when a Create targets a name that is already active.
	ErrRepoExists = errors.New("repo already exists")
	// ErrInvalidName is returned when a repo name fails validation.
	ErrInvalidName = errors.New("invalid repo name")
	// ErrCreateInFlight is returned when a Create is already running for the same name.
	ErrCreateInFlight = errors.New("create already in flight for this name")
	// ErrOriginInUse is returned when a clone/restore would point a second active
	// repo at an origin URL already used by an active repo.
	ErrOriginInUse = errors.New("origin URL already in use by an active repo")
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

// reserveCreate marks name as in-flight, returning ErrCreateInFlight if another
// Create holds it. The returned release func clears the marker.
func (m *Manager) reserveCreate(name string) (func(), error) {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	if _, ok := m.creating[name]; ok {
		return nil, ErrCreateInFlight
	}
	m.creating[name] = struct{}{}
	return func() {
		m.inflightMu.Lock()
		delete(m.creating, name)
		m.inflightMu.Unlock()
	}, nil
}

// Create initialises a new repo on disk per spec, registers it, and (for clone
// mode) attaches the origin and activates sync. Progress is reported via emit.
// The work is bound to ctx; cancellation aborts and removes a partial .db.
func (m *Manager) Create(ctx context.Context, spec CreateSpec, emit func(Event)) (*RepoInstance, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	if !isValidRepoName(spec.Name) {
		return nil, ErrInvalidName
	}
	if m.Get(spec.Name) != nil {
		return nil, ErrRepoExists
	}
	release, err := m.reserveCreate(spec.Name)
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

	switch spec.Mode {
	case "preset", "custom":
		if ierr := m.initLocal(spec, dbPath, emit); ierr != nil {
			cleanup()
			return nil, ierr
		}
	case "clone":
		if spec.Origin == nil || spec.Origin.URL == "" {
			return nil, fmt.Errorf("%w: clone mode requires origin.url", ErrInvalidName)
		}
		if active := m.ActiveRepoWithOrigin(spec.Origin.URL); active != "" {
			return nil, fmt.Errorf("%w: %q", ErrOriginInUse, active)
		}
		if ierr := m.initClone(spec, dbPath, emit); ierr != nil {
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
func (m *Manager) initLocal(spec CreateSpec, dbPath string, emit func(Event)) error {
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
func (m *Manager) initClone(spec CreateSpec, dbPath string, emit func(Event)) error {
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
