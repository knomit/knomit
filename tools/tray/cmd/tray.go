package cmd

import (
	"context"

	"github.com/spf13/cobra"
)

// RootCmd returns the top-level knomit-tray command. Default action: start the tray UI.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "knomit-tray",
		Short: "Knomit system-tray app",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTray(cmd.Context())
		},
	}
	root.AddCommand(windowCmd())
	return root
}

func runTray(ctx context.Context) error {
	// Filled in by later tasks; placeholder for now so the binary compiles.
	<-ctx.Done()
	return nil
}
