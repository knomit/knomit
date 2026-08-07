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
	// Required, with no fallback: reset is destructive and knomit has no default
	// repo to aim it at. Guessing a name here would wipe a repo the user never
	// named.
	cmd.Flags().StringVar(&repoName, "name", "", "repo name to reset (required)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}
