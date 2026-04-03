package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"knomit/internal/app"
	"knomit/internal/config"
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
			return app.RebuildIndex(cmd.Context(), cfg, repoName)
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name")
	return cmd
}
