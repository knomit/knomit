package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"knomit/internal/app"
	"knomit/internal/config"
)

func resetCmd() *cobra.Command {
	var repoName string
	var force bool
	cmd := &cobra.Command{
		Use:   "reset --name <repo>",
		Short: "Delete one repository's database",
		Long: `Removes the named repository's database file (and its WAL/SHM sidecars).

--name is required: knomit has no default repository, so there is nothing
sensible to reset without one. Run "knomit serve" and list /api/v1/repos, or
look in <home>/repos/, to see which names exist.

On a backup-enabled instance reset refuses without --force, because deleting the
file is not a local-only act there: the registry still lists the repo, so the
next boot restores it from the replica, and doing this to a RUNNING server
deletes a database out from under a live replica.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if repoName == "" {
				return fmt.Errorf("--name is required: name the repo to reset (knomit has no default repo)")
			}
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			// Refused before anything is removed. On a replicated instance the
			// obvious reading of reset — "wipe this repo's local database and
			// let it rebuild" — is not what happens, and neither of the two real
			// outcomes is one to discover by accident.
			if cfg.Backup.Enabled && !force {
				return fmt.Errorf("backup is enabled: this instance replicates to %s, so resetting %q is not "+
					"a local-only action.\n"+
					"  With the server STOPPED, the repo is still in the registry, so the next boot restores this "+
					"database straight back from the replica — the reset appears to work and changes nothing.\n"+
					"  With the server RUNNING, the file is deleted underneath a live replica, and the local "+
					"database and its backup can disagree from that moment on.\n"+
					"Re-run with --force if you have weighed both.", cfg.Backup.URL, repoName)
			}
			return app.ResetRepo(cfg, repoName)
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "", "repo name to reset (required)")
	cmd.Flags().BoolVar(&force, "force", false, "proceed even when backup is enabled")
	return cmd
}
