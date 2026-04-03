package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"knomit/internal/config"
	"knomit/internal/identity"
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
			keyPath := cfg.Remote.SSHKey
			if keyPath == "" {
				keyPath = filepath.Join(cfg.Home, "id_ed25519")
			}
			_, keyFingerprint, err := identity.EnsureKeyPair(keyPath)
			if err != nil {
				return fmt.Errorf("ensure keypair: %w", err)
			}
			agentBranch := identity.AgentBranch(keyFingerprint)
			dbPath := filepath.Join(cfg.Home, "repos", repoName+".db")
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer svc.Close()
			if err := svc.OpenRepo(); err != nil {
				return fmt.Errorf("open git: %w", err)
			}
			idx := svc.Index()
			if err := idx.Sync(context.Background(), svc, agentBranch); err != nil {
				return fmt.Errorf("rebuild: %w", err)
			}
			log.Info().Str("repo", repoName).Msg("Index rebuilt successfully")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name")
	return cmd
}
