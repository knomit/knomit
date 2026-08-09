package cmd

import "github.com/spf13/cobra"

// RootCmd builds the top-level cobra command with all subcommands registered.
func RootCmd() *cobra.Command {
	root := &cobra.Command{Use: "knomit", Short: "Git-backed knowledge base"}
	root.AddCommand(serveCmd())
	root.AddCommand(verifyCmd())
	root.AddCommand(migrateRegistryCmd())
	root.AddCommand(warmModelsCmd())
	root.AddCommand(versionCmd())
	return root
}
