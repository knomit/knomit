package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"knomit/internal/app"
	"knomit/internal/config"
)

func initCmd() *cobra.Command {
	var ontologyPath string
	var ontologyPreset string
	var repoName string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialise a new knomit repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			return app.InitRepo(cfg, repoName, ontologyPath, ontologyPreset)
		},
	}
	cmd.Flags().StringVar(&ontologyPath, "ontology", "", "path to custom ontology YAML file")
	cmd.Flags().StringVar(&ontologyPreset, "ontology-preset", "", "embedded ontology preset name (default|code)")
	cmd.MarkFlagsMutuallyExclusive("ontology", "ontology-preset")
	cmd.Flags().StringVar(&repoName, "name", config.DefaultRepoName, "repo name")
	return cmd
}
