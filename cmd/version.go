package cmd

import (
	"github.com/spf13/cobra"

	"knomit/internal/version"
)

// versionCmd builds the `knomit version` subcommand. It prints the full build
// version (e.g. 0.5.0.2a7ae9d) injected at build time via -ldflags.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println(version.String())
			return nil
		},
	}
}
