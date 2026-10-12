//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/serveraddr"
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
	// "Not absolute" is three separate checks on Windows, and filepath.IsAbs
	// alone is only the first of them:
	//
	//	C:\etc\passwd   IsAbs        -> rejected by IsAbs
	//	\etc\passwd     rooted       -> IsAbs says FALSE (no volume), but it
	//	                                resolves against the current drive
	//	C:passwd        drive-rel    -> IsAbs says FALSE, resolves against the
	//	                                current directory ON drive C
	//	\\srv\share\x   UNC          -> another machine entirely
	//
	// None of those is a path relative to the exports dir, which is the only
	// thing this parameter is allowed to be. On Unix IsAbs already covers the
	// case and the extra checks never fire: ToSlash is a no-op there, so a file
	// genuinely named "\x" stays a legal relative name.
	//
	// The containment check below would catch all of these anyway — this is
	// about REFUSING rather than silently rewriting the caller's path into
	// something else inside the sandbox.
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" || strings.HasPrefix(filepath.ToSlash(name), "/") {
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
// the knomit API. That boundary is why reading and writing settings lives here
// rather than on the HTTP API: knomit.toml holds credentials, and the API is
// reachable by anything on the machine.
//
// Every method has a POINTER receiver: the service is registered as
// &NativeService{} and Wails binds the pointer's method set; installMu makes
// copying the struct a vet error.
type NativeService struct {
	// configPath is <cfg.Home>/knomit.toml — the only file config.Load reads.
	configPath string
	// logPath is the SERVER's log file (resolveLogFile): what Reveal opens and
	// the Settings dialog names when knomit.toml does not name one.
	logPath string
	// desktopLogPath is the desktop shell's own log, which SaveSettings
	// rebuilds the logger onto.
	desktopLogPath string
	// lockPath is this home's server.json: the server's own account of its
	// port and listeners, which GetSettings and GetIdentity read.
	lockPath  string
	autostart autostart.Toggler

	// installMu serialises InstallBundle: see there.
	installMu sync.Mutex

	// revealInFileManager opens a path's containing directory in the OS file
	// manager. Defaults to the platform's package-level revealInFileManager;
	// overridden in tests to check RevealLogFile forwards the right path
	// without actually shelling out.
	revealInFileManager func(string) error
	// restartServer stops the server the desktop spawned and waits for its
	// replacement (supervisor.restartServer). Wired in app.go; nil in tests
	// that do not exercise RestartApp.
	restartServer func(context.Context) error
}

// newNativeService builds the service. configPath is <home>/knomit.toml, from
// which the home's server.json is derived; logPath is the server's log file,
// from which the desktop's own sibling log is derived. restartServer is wired
// separately once the supervisor exists — see app.go.
func newNativeService(configPath, logPath string, tog autostart.Toggler) *NativeService {
	return &NativeService{
		configPath:          configPath,
		logPath:             logPath,
		desktopLogPath:      desktopLogFile(logPath),
		lockPath:            config.LockfilePathFor(filepath.Dir(configPath)),
		autostart:           tog,
		revealInFileManager: revealInFileManager,
	}
}

// serverLock reads this home's server.json; ok is false when there is none
// (no server running, or one that has not finished binding its listeners).
func (n *NativeService) serverLock() (serveraddr.LockInfo, bool) {
	info, err := serveraddr.ReadLock(n.lockPath)
	return info, err == nil
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
// that field cannot be changed by saving. The bound port is the running
// server's own record (server.json `port`, 0 while none is up or when it has
// no TCP listener).
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
	effective := 0
	if info, ok := n.serverLock(); ok {
		effective = info.Port
	}

	return Settings{
		Port:            cfg.Port,
		LogLevel:        resolved.Level,
		LogFormat:       resolved.Format,
		StartAtLogin:    on,
		TLSAddr:         cfg.TLS.Addr,
		EffectivePort:   effective,
		ConfigPath:      n.configPath,
		LogFilePath:     resolved.File,
		OverriddenByEnv: envOverrides(os.Getenv),
	}, nil
}

// SaveSettings persists the settings and applies what the desktop itself can
// apply: the desktop shell's own log level and format. The SERVER picks up
// the file on its next start or reload (applying it live from Settings is the
// next PR's); the UI tells the user a restart is needed for the port.
//
// Gated to the Settings window: a save can set [tls].addr, which opens a
// network port (see callerIsSettings).
func (n *NativeService) SaveSettings(ctx context.Context, s Settings) error {
	if !callerIsSettings(ctx) {
		return errNotSettingsWindow
	}
	if err := applySettings(s, n.configPath, n.autostart, envOverrides(os.Getenv)); err != nil {
		return err
	}
	// Rebuilt from a FRESH config.Load rather than from s, so the logger ends
	// up exactly as the next launch would have it: the rotation keys the
	// dialog does not edit survive, and an exported KNOMIT_LOG_LEVEL still
	// wins, matching what OverriddenByEnv told the user.
	lc := config.LogConfig{Level: s.LogLevel, Format: s.LogFormat}
	if cfg, cerr := config.Load(); cerr == nil {
		lc = cfg.Log
	}
	// A failure here does not fail the save: the file is already correct and the
	// next launch will pick it up.
	if err := applyLogConfig(lc, n.desktopLogPath); err != nil {
		log.Warn().Err(err).Msg("settings saved but the logger was not rebuilt")
	}
	return nil
}

// RestartApp restarts the knomit SERVER the desktop spawned — not the app:
// the desktop keeps running and its window reconnects once the replacement
// answers. It returns when the new server answers (or with the reason it did
// not). Refused for a server started outside the app (attach mode: it is not
// ours to stop), named "started outside the app (pid N)".
//
// The binding keeps its name so the Settings window's Call.ByName string is
// unchanged; the Settings UI that says "Restart server" is the next PR's.
//
// Gated to the Settings window, like everything that changes what the server
// listens on (kb invariant desktop/fleet/install-surface): a restart applies
// a [tls].addr or port the Settings window wrote.
func (n *NativeService) RestartApp(ctx context.Context) error {
	if !callerIsSettings(ctx) {
		return errNotSettingsWindow
	}
	if n.restartServer == nil {
		return errors.New("no server to restart")
	}
	return n.restartServer(ctx)
}

// RevealLogFile opens the server log's containing directory in the OS file
// manager, for the history no live view shows: Manage → Logs replays a bounded
// ring of recent lines, and rotated files are not on it at all. The desktop's
// own knomit-desktop.log sits beside it.
func (n *NativeService) RevealLogFile() error {
	return n.revealInFileManager(n.logPath)
}
