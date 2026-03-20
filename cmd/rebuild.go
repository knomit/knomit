package main

import (
	"fmt"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"knomit/internal/config"
	"knomit/internal/git"
	"knomit/internal/store"
)

func rebuildCmd() *cobra.Command {
	var repoName string
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the search index from scratch",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbPath := filepath.Join(cfg.Home, "repos", repoName+".db")
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer svc.Close()
			gs, err := git.OpenWithStorer(svc.GitStorer())
			if err != nil {
				return fmt.Errorf("open git: %w", err)
			}
			idx := svc.Index()
			if err := idx.Sync(gs, gs.Branch()); err != nil {
				return fmt.Errorf("rebuild: %w", err)
			}
			log.Info().Str("repo", repoName).Msg("Index rebuilt successfully")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name")
	return cmd
}
