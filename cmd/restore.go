package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/backup"
	"knomit/internal/config"
	"knomit/internal/homelock"
)

// restoreCmd builds the `knomit restore` subcommand.
//
// This is the ONLY path in knomit permitted to overwrite an existing database.
// The automatic boot-time restore deliberately fills absences and nothing else,
// which is what makes it safe to run unattended — and is also why it cannot
// help a database that is present but corrupt. That case is this command's
// reason to exist, and it is deliberately something a human has to type.
func restoreCmd() *cobra.Command {
	var (
		repoName  string
		control   bool
		timestamp string
		output    string
		instance  string
	)
	cmd := &cobra.Command{
		Use:   "restore (--repo <name> | --control)",
		Short: "Restore a database from the backup replica, replacing the local copy",
		Long: `Restore one database from the configured backup replica.

This is the ONLY command permitted to overwrite an existing database. The
automatic restore that runs at boot fills absences only, so it will not touch a
database that is present — including one that is present and CORRUPT. This is
the recovery path for that.

Run it against a STOPPED server. Restoring underneath a running knomit replaces
a file two processes are holding open, and corrupts both copies — so this
command claims KNOMIT_HOME for its duration and refuses outright if a live
server holds it. A server that CRASHED does not hold it, because the kernel
releases the claim when the process dies; recovery is exactly when this command
is needed.

The target is never guessed: pass --repo <name> or --control. --timestamp
restores the state as of a point in time instead of the latest; --output writes
somewhere other than the live location, which is the safe way to inspect a
backup before committing to it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Flags first, before config.Load and long before backup.Open —
			// which spawns the agent and probes the object store. Reporting a
			// missing flag after seconds of setup, or masking it behind an
			// unrelated config error, is the failure cmd/reset and cmd/verify
			// already avoid.
			switch {
			case control && repoName != "":
				return fmt.Errorf("--repo and --control name different databases; pass exactly one")
			case !control && repoName == "":
				return fmt.Errorf("nothing to restore: pass --repo <name> for a repository, " +
					"or --control for control.db (knomit has no default repo)")
			}
			var at time.Time
			if timestamp != "" {
				var err error
				at, err = time.Parse(time.RFC3339, timestamp)
				if err != nil {
					return fmt.Errorf("parse --timestamp (want RFC3339, e.g. 2026-07-28T12:00:00Z): %w", err)
				}
			}

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if !cfg.Backup.Enabled {
				return fmt.Errorf("backup is not enabled, so there is no replica to restore from " +
					"(set backup.enabled and backup.url, or KNOMIT_BACKUP_ENABLED and KNOMIT_BACKUP_URL)")
			}
			// The instance prefix decides WHOSE replica is read. Overriding it
			// is how one instance restores from another's backup — a migration,
			// or a rebuild under a new name.
			if instance != "" {
				cfg.Backup.Instance = instance
			}

			name := "control"
			dst := filepath.Join(cfg.Home, "control.db")
			if !control {
				name = repoName
				dst = filepath.Join(cfg.Home, "repos", repoName+".db")
			}
			if output != "" {
				dst = output
			}

			// Claimed BEFORE the agent is spawned, and HELD across the restore
			// rather than merely probed. Checking and releasing would leave a
			// window for a server to start mid-restore, which is the same
			// two-writers accident with better timing.
			lock, err := homelock.Acquire(cfg.Home)
			if err != nil {
				if errors.Is(err, homelock.ErrHeld) {
					return fmt.Errorf("%w.\n"+
						"Restoring underneath a running server replaces a database file both processes hold "+
						"open, clears the WAL the running one is writing, and deletes the replication state "+
						"its backup agent is using. Stop the server and run this again.\n"+
						"(A server that CRASHED does not hold this claim — the kernel releases it when the "+
						"process dies — so this will not stand in the way of recovery.)", err)
				}
				return err
			}
			defer lock.Release()

			m, err := backup.Open(cfg.Backup, cfg.Home)
			if err != nil {
				return err
			}
			// context.Background(), not cmd.Context(): the agent's shutdown runs
			// a final replica sync per database, and a cancelled context would
			// abort it. Nothing is tracked here, so it is cheap either way.
			defer m.Close(context.Background())

			if err := m.RestoreTo(cmd.Context(), name, dst, at); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %s to %s\n", name, dst)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "repository to restore")
	cmd.Flags().BoolVar(&control, "control", false, "restore control.db (the repo registry) instead of a repository")
	cmd.Flags().StringVar(&timestamp, "timestamp", "", "point in time to restore to, RFC3339 (default: the latest state)")
	cmd.Flags().StringVar(&output, "output", "", "destination path (default: the live location under KNOMIT_HOME)")
	cmd.Flags().StringVar(&instance, "instance", "", "read another instance's replica prefix instead of this one's")
	return cmd
}
