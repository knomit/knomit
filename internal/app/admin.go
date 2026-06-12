package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
)

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
