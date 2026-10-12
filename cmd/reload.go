package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/serveraddr"
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
	if ctx == nil {
		ctx = context.Background()
	}
	home, err := config.ResolveHome()
	if err != nil {
		return err
	}
	path := config.LockfilePathFor(home)
	info, err := serveraddr.ReadLock(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no running knomit serve for %s (no %s)", home, path)
	}
	if err != nil {
		return err
	}
	if info.Home != "" && info.Home != home {
		return fmt.Errorf("server.json at %s belongs to the data root %s, not %s", path, info.Home, home)
	}
	if !info.Reload {
		return fmt.Errorf("the server (pid %d, %s) does not listen for reload: restart it to apply configuration changes", info.PID, info.Version)
	}
	if info.Socket != "" {
		// The pid in the file is only as good as the file: ask the listener
		// that file names whether anything is there, before signalling.
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, derr := auth.DialLocal(dctx, info.Socket, 2*time.Second)
		cancel()
		if derr != nil {
			return fmt.Errorf("server.json names pid %d, but its local listener does not answer (%v); not signalling it", info.PID, derr)
		}
		conn.Close()
	}
	if err := signalReload(home, info.PID); err != nil {
		return err
	}
	if wait <= 0 {
		fmt.Fprintln(out, "reload requested")
		return nil
	}
	deadline := time.Now().Add(wait)
	for {
		cur, rerr := serveraddr.ReadLock(path)
		if rerr == nil && cur.PID == info.PID && cur.Generation > info.Generation {
			printReload(out, cur)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reload requested, but the server did not acknowledge within %s (generation still %d); "+
				"its log says why if the configuration did not load", wait, info.Generation)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
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
		fmt.Fprintln(out, line)
	}
	if len(cur.RestartNeeded) > 0 {
		fmt.Fprintf(out, "  needs a restart: %v\n", cur.RestartNeeded)
	}
}
