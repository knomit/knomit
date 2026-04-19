package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"knomit/tools/tray/internal/autostart"
	"knomit/tools/tray/internal/lockfile"
	"knomit/tools/tray/internal/paths"
	"knomit/tools/tray/internal/singleinstance"
	"knomit/tools/tray/internal/supervisor"
	"knomit/tools/tray/internal/trayui"
)

// version is overridden at build time via -ldflags "-X knomit/tools/tray/cmd.version=X.Y.Z".
var version = "0.1.0-dev"

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
	lockPath, err := paths.LockfilePath()
	if err != nil {
		return err
	}
	if err := singleinstance.Acquire(lockPath); err != nil {
		if errors.Is(err, singleinstance.ErrAlreadyRunning) {
			fmt.Fprintln(os.Stderr, "knomit-tray is already running.")
			return nil
		}
		return err
	}

	knomitBin, err := locateKnomit()
	if err != nil {
		return err
	}
	trayBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}

	logsDir, err := paths.LogsDir()
	if err != nil {
		return err
	}

	sup := supervisor.New(supervisor.Config{
		Binary:  knomitBin,
		Port:    0, // supervisor picks a free port
		LogsDir: logsDir,
	})
	if err := sup.Start(); err != nil {
		return fmt.Errorf("start serve: %w", err)
	}
	if err := sup.WaitForHealthy(10 * time.Second); err != nil {
		sup.Stop(3 * time.Second)
		return err
	}

	info := lockfile.Info{PID: os.Getpid(), Port: sup.Port(), Version: version}
	if err := lockfile.Write(lockPath, info); err != nil {
		sup.Stop(3 * time.Second)
		return fmt.Errorf("write lockfile: %w", err)
	}
	defer lockfile.Remove(lockPath)

	as := autostart.New()

	// Run the tray UI. This blocks until Quit is clicked.
	trayui.Run(trayui.Deps{
		Supervisor: sup,
		Autostart:  as,
		TrayBinary: trayBin,
	})

	// Tray exited; ensure child is stopped.
	if err := sup.Stop(5 * time.Second); err != nil {
		log.Warn().Err(err).Msg("supervisor stop on exit failed")
	}
	_ = ctx // reserved for future cancellation wiring
	return nil
}

// locateKnomit finds the `knomit` binary in this order:
//  1. $KNOMIT_BIN
//  2. same directory as knomit-tray
//  3. $PATH
func locateKnomit() (string, error) {
	if env := os.Getenv("KNOMIT_BIN"); env != "" {
		return env, nil
	}
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "knomit")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("knomit"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("knomit binary not found (set KNOMIT_BIN, place next to knomit-tray, or add to PATH)")
}
