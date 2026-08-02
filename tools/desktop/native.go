//go:build desktop

package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/tools/desktop/internal/autostart"
	"knomit/tools/desktop/internal/paths"
)

// exportsDir is the ONLY directory NativeService is permitted to write into:
// <StateDir>/exports. Confining writes here is what keeps the Wails binding
// from being an arbitrary-file-write primitive reachable from the webview — a
// single UI XSS could otherwise overwrite, say, the user's shell rc and gain
// code execution. The directory is created lazily on first write.
func exportsDir() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "exports"), nil
}

// writeFile writes contents to name inside the exports dir and returns the
// absolute path written. name is treated as a path RELATIVE to the exports dir;
// absolute paths are rejected and any "../" traversal is neutralised so a write
// can never escape the sandbox. Exercised directly by tests.
func writeFile(name, contents string) (string, error) {
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("name must be relative to the exports dir: %q", name)
	}
	base, err := exportsDir()
	if err != nil {
		return "", err
	}
	// Clean("/"+name) collapses any leading "../" against root, so the joined
	// result can never climb above base. The filepath.Rel check below is
	// defense-in-depth, making the containment invariant explicit.
	target := filepath.Join(base, filepath.Clean("/"+name))
	if rel, relErr := filepath.Rel(base, target); relErr != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("name escapes the exports dir: %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("create exports dir: %w", err)
	}
	if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
		return "", err
	}
	return target, nil
}

// NativeService is the Wails-bound service exposing native OS actions to the
// React UI. Reachable only via Wails IPC from the embedded window — never over
// the looknomitck API port. That boundary is why reading and writing settings
// lives here rather than on the HTTP API: knomit.toml holds credentials, and
// the API is reachable by anything on the machine.
//
// Every method has a POINTER receiver: the service is registered as
// &NativeService{} and Wails binds the pointer's method set, so a value
// receiver would still bind — but the mutex below makes copying the struct a
// vet error, and the effective port must be read from the one instance the
// boot goroutine writes to.
type NativeService struct {
	// configPath is <cfg.Home>/knomit.toml — the file config.findConfigFile
	// falls through to, and the one the macOS bundle does not ship.
	configPath string
	// logPath is the desktop's default log file, used to fill the gap when
	// knomit.toml does not name one.
	logPath   string
	autostart autostart.Toggler

	// mu guards effectivePort ONLY. It is written by the boot watcher
	// goroutine in run() once the server has bound its port, and read by
	// GetSettings on the UI thread, which can be called before that happens.
	mu            sync.Mutex
	effectivePort int

	// relaunchTarget resolves WHERE to relaunch (the .app bundle on darwin, the
	// executable path elsewhere) without spawning anything. Defaults to the
	// platform's package-level relaunchTarget. It is pure and cheap, and
	// RestartApp calls it FIRST, before releaseInstance: this is what lets a
	// resolution failure (dev build with no bundle, or one moved/deleted)
	// return an error WITHOUT having torn this instance's server down first.
	relaunchTarget func() (string, error)
	// relaunch spawns a fresh instance at the path relaunchTarget resolved.
	// Defaults to the platform's package-level relaunch
	// (restart_darwin.go / restart_others.go); overridden in tests so
	// RestartApp's ordering can be verified without actually shelling out.
	relaunch func(target string) error
	// revealInFileManager opens a path's containing directory in the OS file
	// manager. Defaults to the platform's package-level revealInFileManager;
	// overridden in tests to check RevealLogFile forwards the right path
	// without actually shelling out.
	revealInFileManager func(string) error
	// releaseInstance synchronously tears down THIS process's server AND
	// guarantees the single-instance lockfile is gone by the time it returns
	// — even if the server teardown itself times out (serverBoot.stop's
	// stopGrace), which would otherwise leave stopFn — and the lockfile
	// removal inside it — never called. It MUST run before relaunch: the
	// lockfile check the new process performs on startup
	// (singleinstance.Acquire) is a one-shot check, not a wait — if this
	// process still holds the lock when the new one checks, the new instance
	// sees ErrAlreadyRunning and exits immediately, and the "restart" silently
	// does nothing. Wired in app.go to call serverBoot.stop (idempotent, so
	// calling it here and again on the eventual OnShutdown costs nothing) AND
	// an explicit lockfile.Remove that does not depend on stopFn having run.
	//
	// That explicit removal is safe only as long as serverBoot.stop's
	// sync.Once means this process's OnShutdown-driven call is always a no-op
	// after this one runs first: lockfile.Remove is path-based with no PID
	// ownership check (see internal/lockfile.Remove), so a stop() that became
	// re-entrant/re-runnable could race and delete a REPLACEMENT instance's
	// freshly-written lockfile instead of this one's. Nothing currently
	// guards against that beyond stop()'s Once.
	releaseInstance func()
	// onRestart quits this instance once the replacement has been spawned.
	// Wired to wapp.Quit in app.go (set after the Wails app exists, since
	// NativeService is constructed first). Left nil in tests that do not
	// exercise RestartApp.
	onRestart func()
}

// newNativeService builds the service with the paths and OS toggler it needs.
// The effective port is not known yet — see setEffectivePort. releaseInstance
// and onRestart are wired separately once their dependencies (the server boot
// handle and the Wails app) exist — see app.go.
func newNativeService(configPath, logPath string, tog autostart.Toggler) *NativeService {
	return &NativeService{
		configPath:          configPath,
		logPath:             logPath,
		autostart:           tog,
		relaunchTarget:      relaunchTarget,
		relaunch:            relaunch,
		revealInFileManager: revealInFileManager,
	}
}

// setEffectivePort records the port the server actually bound. Called from the
// boot watcher goroutine, hence the mutex.
func (n *NativeService) setEffectivePort(port int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.effectivePort = port
}

// currentEffectivePort returns the port the server actually bound, or 0 while
// the server is still booting. Unexported deliberately: every EXPORTED method
// on this type becomes a Wails-bound IPC entry point, and the form already
// gets this value inside Settings.
func (n *NativeService) currentEffectivePort() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.effectivePort
}

// WriteFile writes contents to a file named within the app's exports directory
// (<StateDir>/exports) and returns the absolute path written. name is confined
// to that directory; absolute paths and "../" traversal are rejected, so the UI
// can never use this to write outside the sandbox.
func (n *NativeService) WriteFile(name, contents string) (string, error) {
	return writeFile(name, contents)
}

// GetSettings returns the current settings plus the context the form needs:
// which environment variables are overriding the file, and what port is
// actually bound.
//
// The values come from config.Load, i.e. AFTER the env layer, so an exported
// KNOMIT_PORT is what the form shows — matched by OverriddenByEnv telling it
// that field cannot be changed by saving.
func (n *NativeService) GetSettings() (Settings, error) {
	cfg, err := config.Load()
	if err != nil {
		return Settings{}, fmt.Errorf("load config: %w", err)
	}
	// A toggler that cannot answer is reported as "off" rather than failing the
	// whole dialog: the other five fields are still worth showing.
	on, terr := n.autostart.Enabled()
	if terr != nil {
		log.Warn().Err(terr).Msg("start at login: current state unknown")
	}
	resolved := desktopLogConfig(cfg.Log, n.logPath)

	return Settings{
		Port:            cfg.Port,
		LogLevel:        resolved.Level,
		LogFormat:       resolved.Format,
		StartAtLogin:    on,
		EffectivePort:   n.currentEffectivePort(),
		ConfigPath:      n.configPath,
		LogFilePath:     resolved.File,
		OverriddenByEnv: envOverrides(os.Getenv),
	}, nil
}

// SaveSettings persists the settings and applies what can be applied without a
// restart. The PORT cannot: it is bound once at boot, and rebinding would
// strand every connected MCP client, which caches the port at startup. The UI
// is responsible for telling the user that.
func (n *NativeService) SaveSettings(s Settings) error {
	if err := applySettings(s, n.configPath, n.autostart, envOverrides(os.Getenv)); err != nil {
		return err
	}
	// Level and format take effect immediately — there is no reason to make the
	// user restart for them. Rebuilt from a FRESH config.Load rather than from s,
	// so the logger ends up exactly as the next launch would have it: the
	// rotation keys the dialog does not edit survive, and an exported
	// KNOMIT_LOG_LEVEL still wins, matching what OverriddenByEnv told the user.
	lc := config.LogConfig{Level: s.LogLevel, Format: s.LogFormat}
	if cfg, cerr := config.Load(); cerr == nil {
		lc = cfg.Log
	}
	// A failure here does not fail the save: the file is already correct and the
	// next launch will pick it up.
	if err := applyLogConfig(lc, n.logPath); err != nil {
		log.Warn().Err(err).Msg("settings saved but the logger was not rebuilt")
	}
	return nil
}

// RestartApp relaunches Knomit. Used after a port change, which cannot take
// effect without rebinding the listener.
//
// Order matters, in both directions:
//
//  1. relaunchTarget resolves FIRST, before anything else runs. It is pure
//     (no side effects), and its only realistic failure — a dev build with no
//     bundle to reopen, or a bundle moved/deleted since this process started
//     — must be reported without having torn anything down. Resolving after
//     releaseInstance would mean a dev build's Restart button kills its own
//     server and then fails, leaving a zombie tray with a dead API behind it.
//  2. releaseInstance runs SECOND and is allowed to block briefly (it drains
//     the HTTP server and removes the lockfile) so the lockfile is gone
//     before the replacement process ever checks it — see the field comment
//     on releaseInstance for why spawning first would race.
//  3. onRestart (wapp.Quit) runs LAST, after the replacement is confirmed
//     spawned, so a failed relaunch leaves this instance running rather than
//     quitting into nothing.
func (n *NativeService) RestartApp() error {
	target, err := n.relaunchTarget()
	if err != nil {
		return fmt.Errorf("resolve relaunch target: %w", err)
	}
	if n.releaseInstance != nil {
		n.releaseInstance()
	}
	if err := n.relaunch(target); err != nil {
		return fmt.Errorf("relaunch: %w", err)
	}
	if n.onRestart != nil {
		n.onRestart()
	}
	return nil
}

// RevealLogFile opens the log file's containing directory in the OS file
// manager, for the history the Logs window's bounded tail does not show.
func (n *NativeService) RevealLogFile() error {
	return n.revealInFileManager(n.logPath)
}

// portFromBaseURL extracts the port from an API base URL like
// "http://127.0.0.1:19278", returning 0 if there is none to extract. The bound
// port reaches the UI only through that string (see bootKnomit), and a
// malformed one must cost the form its EffectivePort, nothing more.
func portFromBaseURL(base string) int {
	u, err := url.Parse(base)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return port
}
