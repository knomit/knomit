package cmd

import "github.com/spf13/cobra"

// RootCmd builds the top-level cobra command with all subcommands registered.
//
// There is deliberately no `reset` command. It deleted a repo's .db file
// directly and predated the control.db registry, so once the registry became
// authoritative it stopped meaning what it said: the row survived the delete,
// and the next boot either re-cloned the repo straight back from its recorded
// origin — reset as a no-op with extra steps — or logged it forever as
// registered-but-unrebuildable.
//
// Removing a repo goes through the registry instead: DELETE /api/v1/repos/{repo}
// archives it, retiring the active row, and DELETE /api/v1/archived/{id} purges
// that archive for good.
func RootCmd() *cobra.Command {
	root := &cobra.Command{Use: "knomit", Short: "Git-backed knowledge base"}
	root.AddCommand(serveCmd())
	root.AddCommand(verifyCmd())
	root.AddCommand(warmModelsCmd())
	root.AddCommand(versionCmd())
	return root
}
