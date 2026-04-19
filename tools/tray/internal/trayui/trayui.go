//go:build darwin

// Package trayui owns the systray menu and wires menu events to the supervisor.
package trayui

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/rs/zerolog/log"

	"knomit/tools/tray/internal/autostart"
	"knomit/tools/tray/internal/lockfile"
	"knomit/tools/tray/internal/supervisor"
)

type Deps struct {
	Autostart    autostart.Toggler
	LockfilePath string
	Supervisor   *supervisor.Supervisor
	TrayBinary   string // absolute path to knomit-tray, used to spawn windows
	Version      string
}

// Run blocks running the systray until Quit is clicked.
// When Run returns, the process should exit.
func Run(d Deps) {
	systray.Run(func() { onReady(d) }, func() { onExit(d) })
}

var (
	statusMu sync.Mutex
	status   *systray.MenuItem
)

func onReady(d Deps) {
	systray.SetTemplateIcon(IconBytes(), IconBytes())
	systray.SetTitle("")
	systray.SetTooltip("Knomit")

	status = systray.AddMenuItem("Starting…", "")
	status.Disable()
	systray.AddSeparator()

	openItem := systray.AddMenuItem("Open", "Open the Knomit web UI")
	restartItem := systray.AddMenuItem("Restart server", "Restart the knomit serve process")
	systray.AddSeparator()

	autostartItem := systray.AddMenuItemCheckbox("Start at login", "Launch knomit-tray when you log in", false)
	if enabled, err := d.Autostart.Enabled(); err == nil && enabled {
		autostartItem.Check()
	}

	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit knomit", "Stop the server and quit")

	// Reflect supervisor state in the status line.
	d.Supervisor.OnStateChange(func(s supervisor.State) {
		setStatus(d, s)
	})
	setStatus(d, d.Supervisor.State())

	go func() {
		for {
			select {
			case <-openItem.ClickedCh:
				if err := spawnWindow(d); err != nil {
					log.Error().Err(err).Msg("spawn window failed")
				}
			case <-restartItem.ClickedCh:
				restart(d)
			case <-autostartItem.ClickedCh:
				toggleAutostart(d, autostartItem)
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit(d Deps) {
	if err := d.Supervisor.Stop(5 * time.Second); err != nil {
		log.Warn().Err(err).Msg("supervisor stop failed")
	}
}

func setStatus(d Deps, s supervisor.State) {
	statusMu.Lock()
	defer statusMu.Unlock()
	if status == nil {
		return
	}
	switch s {
	case supervisor.StateRunning:
		status.SetTitle(fmt.Sprintf("Running on :%d", d.Supervisor.Port()))
	case supervisor.StateCrashed:
		status.SetTitle("Crashed — click Restart server")
	default:
		status.SetTitle("Stopped")
	}
}

func spawnWindow(d Deps) error {
	url := d.Supervisor.URL()
	if url == "" {
		return fmt.Errorf("server not running")
	}
	cmd := exec.Command(d.TrayBinary, "window", "--url", url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap asynchronously so the child doesn't linger as a zombie.
	// The window lives independently; we don't care about its exit status.
	go func() { _ = cmd.Wait() }()
	return nil
}

// restart stops the server and brings it back up, then republishes the
// lockfile in case the port changed. Runs asynchronously so it doesn't
// block the systray event loop.
func restart(d Deps) {
	go func() {
		if err := d.Supervisor.Stop(3 * time.Second); err != nil {
			log.Warn().Err(err).Msg("stop failed during restart")
		}
		if err := d.Supervisor.Start(); err != nil {
			log.Error().Err(err).Msg("restart failed")
			return
		}
		if err := d.Supervisor.WaitForHealthy(10 * time.Second); err != nil {
			log.Error().Err(err).Msg("serve did not become healthy after restart")
			return
		}
		info := lockfile.Info{
			PID:     os.Getpid(),
			Port:    d.Supervisor.Port(),
			Version: d.Version,
		}
		if err := lockfile.Write(d.LockfilePath, info); err != nil {
			log.Error().Err(err).Msg("rewrite lockfile after restart")
		}
	}()
}

func toggleAutostart(d Deps, item *systray.MenuItem) {
	if item.Checked() {
		if err := d.Autostart.Disable(); err != nil {
			log.Error().Err(err).Msg("disable autostart failed")
			return
		}
		item.Uncheck()
	} else {
		if err := d.Autostart.Enable(); err != nil {
			log.Error().Err(err).Msg("enable autostart failed")
			return
		}
		item.Check()
	}
}
