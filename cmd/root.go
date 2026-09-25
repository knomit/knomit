package cmd

import "github.com/spf13/cobra"

// RootCmd builds the top-level cobra command with all subcommands registered.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "knomit",
		Short: "Git-backed knowledge base",
		// main.go prints a returned error exactly once and exits 1, so cobra
		// must print nothing: not the error (it would appear twice) and not the
		// usage block (it buries the error under the flag list). Set on the
		// ROOT because cobra consults only the command that ran and the root —
		// a SilenceUsage on a parent such as `grants` never reached
		// `grants add`.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serveCmd())
	root.AddCommand(oauthCmd())
	root.AddCommand(verifyCmd())
	root.AddCommand(migrateRegistryCmd())
	root.AddCommand(warmModelsCmd())
	root.AddCommand(versionCmd())
	root.AddCommand(identityCmd())
	root.AddCommand(grantsCmd())
	return root
}
