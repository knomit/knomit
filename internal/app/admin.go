package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// InitRepo creates and initialises a new knomit repo database.
// At most one of ontologyPath or ontologyPreset may be set. If both are
// empty, the default ontology preset is used. If ontologyPath is set,
// the ontology is loaded from that file. If ontologyPreset is set, the
// matching embedded preset is used.
func InitRepo(cfg config.Config, repoName, ontologyPath, ontologyPreset string) error {
	if ontologyPath != "" && ontologyPreset != "" {
		return fmt.Errorf("--ontology and --ontology-preset are mutually exclusive")
	}

	branch, err := ResolveAgentBranch(cfg)
	if err != nil {
		return err
	}

	var ontology *fact.Ontology
	switch {
	case ontologyPath != "":
		data, err := os.ReadFile(ontologyPath)
		if err != nil {
			return fmt.Errorf("read ontology file: %w", err)
		}
		ontology, err = fact.ParseOntology(data)
		if err != nil {
			return fmt.Errorf("parse ontology: %w", err)
		}
	case ontologyPreset != "":
		ontology, err = fact.OntologyByPreset(ontologyPreset)
		if err != nil {
			return err
		}
	default:
		ontology = fact.DefaultOntology()
	}
	ontologyYAML, err := ontology.Serialize()
	if err != nil {
		return fmt.Errorf("serialize ontology: %w", err)
	}

	dbPath, err := InitRepoOnDiskBytes(cfg, repoName, ontologyYAML, branch)
	if err != nil {
		return err
	}
	fmt.Printf("Initialized knomit repo %q at %s\n", repoName, dbPath)
	return nil
}

// InitRepoOnDiskBytes creates and initialises a new repo database at
// <cfg.Home>/repos/<repoName>.db, seeding domains/ontology.yaml with the
// provided ontology bytes on the given agent branch. Returns the dbPath.
func InitRepoOnDiskBytes(cfg config.Config, repoName string, ontologyYAML []byte, agentBranch string) (string, error) {
	reposDir := filepath.Join(cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return "", err
	}
	dbPath := filepath.Join(reposDir, repoName+".db")
	svc, err := store.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer svc.Close()
	if err := svc.InitRepo(map[string]string{
		"domains/ontology.yaml": string(ontologyYAML),
	}, agentBranch); err != nil {
		return "", fmt.Errorf("init git: %w", err)
	}
	return dbPath, nil
}

// ResolveAgentBranch ensures the SSH keypair exists and returns the agent
// branch derived from its fingerprint.
func ResolveAgentBranch(cfg config.Config) (string, error) {
	keyPath := cfg.Remote.SSHKey
	if keyPath == "" {
		keyPath = filepath.Join(cfg.Home, "id_ed25519")
	}
	_, keyFingerprint, err := ensureKeyPair(keyPath)
	if err != nil {
		return "", fmt.Errorf("ensure keypair: %w", err)
	}
	return agentBranch(keyFingerprint), nil
}

// ResetRepo removes the database file (and WAL/SHM sidecars) for the named repo.
func ResetRepo(cfg config.Config, repoName string) error {
	dbFile := filepath.Join(cfg.Home, "repos", repoName+".db")
	if err := os.Remove(dbFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", dbFile, err)
	}
	os.Remove(dbFile + "-wal")
	os.Remove(dbFile + "-shm")
	log.Info().Str("repo", repoName).Msg("database removed")
	return nil
}
