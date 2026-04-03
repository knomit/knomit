package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// InitRepo creates and initialises a new knomit repo database.
// If ontologyPath is non-empty the ontology is loaded from that file;
// otherwise the embedded default ontology is used.
func InitRepo(cfg config.Config, repoName, ontologyPath string) error {
	reposDir := filepath.Join(cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return err
	}

	keyPath := cfg.Remote.SSHKey
	if keyPath == "" {
		keyPath = filepath.Join(cfg.Home, "id_ed25519")
	}
	_, keyFingerprint, err := ensureKeyPair(keyPath)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}
	agentBranch := agentBranch(keyFingerprint)

	ontology := fact.DefaultOntology()
	if ontologyPath != "" {
		data, err := os.ReadFile(ontologyPath)
		if err != nil {
			return fmt.Errorf("read ontology file: %w", err)
		}
		ontology, err = fact.ParseOntology(data)
		if err != nil {
			return fmt.Errorf("parse ontology: %w", err)
		}
	}
	ontologyYAML, err := ontology.Serialize()
	if err != nil {
		return fmt.Errorf("serialize ontology: %w", err)
	}

	dbPath := filepath.Join(reposDir, repoName+".db")
	svc, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer svc.Close()

	if err := svc.InitRepo(map[string]string{
		"domains/ontology.yaml": string(ontologyYAML),
	}, agentBranch); err != nil {
		return fmt.Errorf("init git: %w", err)
	}
	fmt.Printf("Initialized knomit repo %q at %s\n", repoName, dbPath)
	return nil
}

// RebuildIndex rebuilds the search index for the named repo from scratch.
func RebuildIndex(ctx context.Context, cfg config.Config, repoName string) error {
	keyPath := cfg.Remote.SSHKey
	if keyPath == "" {
		keyPath = filepath.Join(cfg.Home, "id_ed25519")
	}
	_, keyFingerprint, err := ensureKeyPair(keyPath)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}
	agentBranch := agentBranch(keyFingerprint)

	dbPath := filepath.Join(cfg.Home, "repos", repoName+".db")
	svc, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer svc.Close()

	if err := svc.OpenRepo(); err != nil {
		return fmt.Errorf("open git: %w", err)
	}
	if err := svc.Search().Sync(ctx, agentBranch); err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	log.Info().Str("repo", repoName).Msg("Index rebuilt successfully")
	return nil
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
