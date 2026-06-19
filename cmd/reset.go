package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"knomit/internal/app"
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
			return app.ResetRepo(cfg, repoName)
		},
	}
	cmd.Flags().StringVar(&repoName, "name", config.DefaultRepoName, "repo name to reset")
	return cmd
}
