package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/foundation/config"
	"knomit/internal/foundation/serveraddr"
)

// reloadCmd is the CLI trigger for a running server's reload: the same thing
// as `kill -HUP <pid>` on unix, and the only way short of a restart on
// Windows, where it sets the server's named event. It works on a server
// started by the CLI or by the desktop, for the data root this process
// resolves (KNOMIT_HOME).
//
// It signals nothing but a server that says it listens (server.json
// "reload": true) and whose local listener answers: SIGHUP's default action
// ends a process, so a stale server.json whose pid was reused, or a server
// from before reload existed, must never be signalled.
func reloadCmd() *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Make the running knomit serve re-read its configuration",
		Long: `Make the running knomit serve of this data root re-read knomit.toml and the
environment and apply what can change without a restart: the plaintext HTTP
listener (http, host, port), the mTLS listener ([tls].addr) and [log].level.
Everything else is listed as needing a restart and left alone. A configuration
that does not load keeps the running one.

On unix this is kill -HUP; on Windows it sets the server's reload event.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReload(cmd.Context(), cmd.OutOrStdout(), wait)
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 5*time.Second, "how long to wait for the server to acknowledge (0 = do not wait)")
	return cmd
}

func runReload(ctx context.Context, out io.Writer, wait time.Duration) error {
	home, err := config.ResolveHome()
	if err != nil {
		return err
	}
	cur, err := serveraddr.RequestReload(ctx, home, wait)
	if err != nil {
		// The generation advanced but a listener is not as configured: the
		// outcome is still worth printing before the non-zero exit.
		var le *serveraddr.ReloadListenerError
		if errors.As(err, &le) {
			printReload(out, cur)
		}
		return err
	}
	if wait <= 0 {
		fmt.Fprintln(out, "reload requested")
		return nil
	}
	printReload(out, cur)
	return nil
}

func printReload(out io.Writer, cur serveraddr.LockInfo) {
	fmt.Fprintf(out, "reloaded (generation %d)\n", cur.Generation)
	names := make([]string, 0, len(cur.Listeners))
	for n := range cur.Listeners {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := cur.Listeners[n]
		state := "off"
		if l.Listening != "" {
			state = "listening on " + l.Listening
		}
		line := fmt.Sprintf("  %s: %s", n, state)
		if l.Reason != "" {
			line += " (" + l.Reason + ")"
		}
		if serveraddr.ListenerWaitsForCertificate(l) {
			line += ", " + serveraddr.TLSWaitingForCertificate
		}
		fmt.Fprintln(out, line)
	}
	if len(cur.RestartNeeded) > 0 {
		fmt.Fprintf(out, "  needs a restart: %v\n", cur.RestartNeeded)
	}
}
