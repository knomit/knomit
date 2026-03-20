package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"knomit/internal/config"
)

func resetCmd() *cobra.Command {
	var repoName string
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Wipe all data and start fresh",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbFile := filepath.Join(cfg.Home, "repos", repoName+".db")
			for _, f := range []string{dbFile} {
				if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove %s: %w", f, err)
				}
				// WAL/SHM sidecars
				os.Remove(f + "-wal")
				os.Remove(f + "-shm")
			}

			log.Info().Str("repo", repoName).Msg("database removed")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name to reset")
	return cmd
}
