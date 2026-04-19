//go:build linux

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/tools/tray/internal/lockfile"
	"knomit/tools/tray/internal/paths"
	"knomit/tools/tray/internal/singleinstance"
	"knomit/tools/tray/internal/supervisor"
)

// runTray runs the headless knomit-tray supervisor on Linux. No systray is
// shown; the user opens the UI via `knomit-tray window` (typically wired to
// a .desktop launcher). The process blocks on ctx until SIGINT/SIGTERM.
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

	logsDir, err := paths.LogsDir()
	if err != nil {
		return err
	}

	sup := supervisor.New(supervisor.Config{
		Binary:  knomitBin,
		Port:    0,
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

	log.Info().
		Int("port", sup.Port()).
		Str("url", sup.URL()).
		Msg("knomit running; press Ctrl-C to stop")

	<-ctx.Done()

	if err := sup.Stop(5 * time.Second); err != nil {
		log.Warn().Err(err).Msg("supervisor stop on exit failed")
	}
	return nil
}
