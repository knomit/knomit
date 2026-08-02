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
		Use:   "reset --name <repo>",
		Short: "Delete one repository's database",
		Long: `Removes the named repository's database file (and its WAL/SHM sidecars).

--name is required: knomit has no default repository, so there is nothing
sensible to reset without one. Run "knomit serve" and list /api/v1/repos, or
look in <home>/repos/, to see which names exist.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if repoName == "" {
				return fmt.Errorf("--name is required: name the repo to reset (knomit has no default repo)")
			}
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			return app.ResetRepo(cfg, repoName)
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "", "repo name to reset (required)")
	return cmd
}
