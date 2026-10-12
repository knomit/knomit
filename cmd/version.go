package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"knomit/internal/platform/version"
)

// versionCmd builds the `knomit version` subcommand. It prints the full build
// version (e.g. 0.5.0.2a7ae9d) injected at build time via -ldflags.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// STDOUT, not cmd.Println (cobra's Println writes to stderr):
			// the desktop reads this line from stdout to check that the
			// bundled server is its own build (tools/desktop/serverbin.go).
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return err
		},
	}
}
