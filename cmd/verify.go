package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/store"
)

// verifyCmd builds the `knomit verify` subcommand. It boots the full app to
// open all configured repos through the usual manager path, then runs
// Verify on one or all repos and prints the report.
//
// Exit codes:
//
//	0 clean
//	1 at least one repo had integrity issues
//	2 verify itself errored (bad config, unknown repo, etc.)
func verifyCmd() *cobra.Command {
	var repoName string
	var all bool
	var deep bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Run an integrity check on one or all knomit repositories",
		Long: `Walks the git object chain, SQLite tables, and search index for the
specified repo (or all repos with --all) and reports any structural issues.

For definitive results on a busy repo, stop the agent first — verify is
read-only but acquires per-branch locks one branch at a time, so it does
not produce an atomic snapshot of the whole repo.

Exit codes:
  0   clean
  1   integrity issues found
  2   the verify command itself errored`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			ctx := context.Background()
			a, err := app.New(ctx, cfg, app.Options{})
			if err != nil {
				return fmt.Errorf("init app: %w", err)
			}
			defer a.Close()

			opts := store.VerifyOpts{Deep: deep}
			anyDirty := false
			anyErr := false

			run := func(name string) {
				ri := a.Manager().Get(name)
				if ri == nil {
					fmt.Fprintf(os.Stderr, "repo %q not found\n", name)
					anyErr = true
					return
				}
				report, err := ri.Verify(ctx, opts)
				if err != nil {
					fmt.Fprintf(os.Stderr, "verify %q: %v\n", name, err)
					anyErr = true
					return
				}
				fmt.Print(report.String())
				if !report.IsClean() {
					anyDirty = true
				}
			}

			if all {
				for _, n := range a.Manager().Names() {
					run(n)
				}
			} else {
				if repoName == "" {
					repoName = config.DefaultRepoName
				}
				run(repoName)
			}

			switch {
			case anyErr:
				os.Exit(2)
			case anyDirty:
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "repo name (default: knomit)")
	cmd.Flags().BoolVar(&all, "all", false, "verify all repos")
	cmd.Flags().BoolVar(&deep, "deep", false, "enable deep checks (parses every fact)")
	return cmd
}
