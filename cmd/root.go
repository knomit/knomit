package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

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
		// `grants add`. Execute restores the one line of help worth keeping.
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

// Execute runs root and returns the error main.go prints, or nil.
//
// A command line cobra itself rejects — an unknown command or flag, a wrong
// argument count — gets "Run '<command> --help' for usage." appended, the
// hint the root's SilenceErrors would otherwise drop. An error from a
// command's RunE (a serve boot failure, a refused grant) does not: it is an
// answer, not a usage mistake.
//
// The distinction is structural, not a match on the error text: cobra runs
// PersistentPreRun only after the flags parsed, the command resolved and its
// Args validated, so an error returned before the hook ran is cobra's own.
// No subcommand may define a PersistentPreRun(E) of its own: with
// cobra.EnableTraverseRunHooks off (the default) it would REPLACE this one,
// and every RunE error of that command would wrongly gain the hint
// (TestRoot_NoSubcommandReplacesTheRunHook). MarkFlagRequired and flag-group
// checks run AFTER the hook, so their errors would get no hint; no command
// uses them.
func Execute(ctx context.Context, root *cobra.Command) error {
	reached := false
	root.PersistentPreRun = func(*cobra.Command, []string) { reached = true }
	c, err := root.ExecuteContextC(ctx)
	if err == nil || reached || c == nil {
		return err
	}
	return fmt.Errorf("%w\nRun '%s --help' for usage.", err, c.CommandPath())
}
