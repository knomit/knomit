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
	"time"

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
// &NativeService{} and Wails binds the pointer's method set; saveMu makes
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

	// revealInFileManager opens a path's containing directory in the OS file
	// manager. Defaults to the platform's package-level revealInFileManager;
	// overridden in tests to check RevealLogFile forwards the right path
	// without actually shelling out.
	revealInFileManager func(string) error
	// restartServer stops the server the desktop spawned and waits for its
	// replacement (supervisor.restartServer). Wired in app.go; nil in tests
	// that do not exercise RestartServer.
	restartServer func(context.Context) error
	// isAttached reports whether the server was started outside the app
	// (supervisor.isAttached). Wired in app.go; nil reads as "ours".
	isAttached func() bool
	// desktopVersion is version.String() of this build, shown beside the
	// server's when they differ.
	desktopVersion string

	// requestReload asks the running server to re-read knomit.toml and waits
	// for its acknowledgement. Defaults to serveraddr.RequestReload, the very
	// function `knomit reload` runs: a signal (SIGHUP, or the Windows event)
	// and a read of server.json, never an HTTP route (kb invariant
	// desktop/fleet/install-surface).
	requestReload func(ctx context.Context, home string, wait time.Duration) (serveraddr.LockInfo, error)
	// reloadWait is how long SaveSettings waits for that acknowledgement.
	reloadWait time.Duration
	// restartWait bounds RestartServer, so a replacement that never answers is
	// an error the Settings window can show rather than a button that waits
	// forever.
	restartWait time.Duration
	// saveMu serialises SaveSettings: write, then reload, then ack must not
	// interleave with another save's.
	saveMu sync.Mutex
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
		requestReload:       serveraddr.RequestReload,
		reloadWait:          5 * time.Second,
		restartWait:         75 * time.Second,
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

// ServerState is what the running server (or the lack of one) says about
// itself, for the Settings window. Everything comes from the server's own
// server.json and the supervisor; nothing is guessed from knomit.toml.
type ServerState struct {
	// Running: a server.json exists for this home. False while the server is
	// still starting, or has failed.
	Running bool `json:"running"`
	PID     int  `json:"pid"`
	// Attached: the server was started outside the app (knomit serve in a
	// terminal, a service). The app neither stops nor restarts it.
	Attached bool `json:"attached"`
	// CanRestart: Restart server is available (a server the app spawned).
	CanRestart bool `json:"canRestart"`
	// CanReload: the server listens for a reload request, which is how a
	// save reaches a running server.
	CanReload      bool   `json:"canReload"`
	Version        string `json:"version"`
	DesktopVersion string `json:"desktopVersion"`
	// VersionSkew: the server and this app differ in major.minor (only
	// possible when attached: a spawned server is this build's own).
	VersionSkew bool `json:"versionSkew"`
	// HTTPURL is the plaintext listener's base URL, "" when it is off.
	HTTPURL    string `json:"httpUrl"`
	HTTPReason string `json:"httpReason"`
	// TLSListening is the fleet listener's bound address, "" when not listening.
	TLSListening string `json:"tlsListening"`
	TLSReason    string `json:"tlsReason"`
	Generation   int    `json:"generation"`
	// RestartNeeded names the keys a reload saw change and could not apply.
	RestartNeeded []string `json:"restartNeeded"`
	// ReloadError is the last refused reload ("<time>: <message>"), "" when none.
	ReloadError string `json:"reloadError"`
}

// serverState reads the running server's account of itself.
func (n *NativeService) serverState() ServerState {
	st := ServerState{DesktopVersion: n.desktopVersion, RestartNeeded: []string{}}
	info, ok := n.serverLock()
	if !ok {
		return st
	}
	st.Running = true
	st.PID = info.PID
	st.Version = info.Version
	st.CanReload = info.Reload
	st.HTTPURL = info.HTTP
	st.Generation = info.Generation
	st.ReloadError = info.ReloadError
	if info.RestartNeeded != nil {
		st.RestartNeeded = info.RestartNeeded
	}
	if l, ok := info.Listeners["http"]; ok && l.Reason != "off" {
		st.HTTPReason = l.Reason
	}
	st.TLSListening = info.TLS
	if l, ok := info.Listeners["tls"]; ok && l.Reason != "off" {
		st.TLSReason = l.Reason
	}
	st.Attached = n.isAttached != nil && n.isAttached()
	st.CanRestart = n.restartServer != nil && !st.Attached
	st.VersionSkew = st.Attached && n.desktopVersion != "" && info.Version != "" &&
		versionSkew(n.desktopVersion, info.Version)
	return st
}

// GetSettings returns the current settings plus the context the form needs:
// which environment variables are overriding the file, and what the running
// server says it is listening on.
//
// The values come from config.Load, i.e. AFTER the env layer, so an exported
// KNOMIT_PORT is what the form shows — matched by OverriddenByEnv telling it
// that field cannot be changed by saving. What is actually bound is the
// running server's own record (server.json), in Server.
func (n *NativeService) GetSettings() (Settings, error) {
	cfg, err := config.Load()
	if err != nil {
		return Settings{}, fmt.Errorf("load config: %w", err)
	}
	// A toggler that cannot answer is reported as "off" rather than failing the
	// whole dialog: the other fields are still worth showing.
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
		HTTPScope:       scopeOf(cfg.HTTP, cfg.Host),
		Server:          n.serverState(),
		EffectivePort:   effective,
		ConfigPath:      n.configPath,
		LogFilePath:     resolved.File,
		OverriddenByEnv: envOverrides(os.Getenv),
	}, nil
}

// SaveAck is SaveSettings' answer: the file is written whatever it says (a
// save that could not be written is an error, not an ack); Outcome says what
// became of the running server.
type SaveAck struct {
	// Outcome is one of:
	//   applied         the server reloaded and every listener is as configured
	//   restart_needed  reloaded, but RestartNeeded keys wait for a restart
	//   listener_failed reloaded, but a listener could not bind (Error says
	//                   why); the previous listener keeps serving
	//   refused         the server could not load the file and kept its running
	//                   configuration (Error is its reason)
	//   timeout         the signal was sent, no acknowledgement arrived
	//   not_running     no server to tell: it applies when the server starts
	//   unavailable     the server cannot be told (it does not listen for
	//                   reload, or runs in another session); Error says why
	Outcome       string   `json:"outcome"`
	Generation    int      `json:"generation"`
	HTTPURL       string   `json:"httpUrl"`
	HTTPReason    string   `json:"httpReason"`
	TLSListening  string   `json:"tlsListening"`
	TLSReason     string   `json:"tlsReason"`
	RestartNeeded []string `json:"restartNeeded"`
	Error         string   `json:"error"`
}

// SaveSettings persists the settings, applies what the desktop itself applies
// (the shell's own log level and format) and then tells the running server to
// re-read knomit.toml, waiting for its acknowledgement: the listeners it now
// has, the keys that need a restart, or why it could not.
//
// Gated to the Settings window: a save can set http, host and [tls].addr, which
// open network ports (see callerIsSettings). The reload is the same call
// `knomit reload` makes, not a route: invariant desktop/fleet/install-surface.
func (n *NativeService) SaveSettings(ctx context.Context, s Settings) (SaveAck, error) {
	if !callerIsSettings(ctx) {
		return SaveAck{}, errNotSettingsWindow
	}
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	if err := applySettings(s, n.configPath, n.autostart, envOverrides(os.Getenv)); err != nil {
		return SaveAck{}, err
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
	return n.reloadServer(ctx), nil
}

// reloadServer tells the running server to re-read the file and turns the
// answer into an ack. It runs after EVERY save, not only after one that
// changed a listener: a retry after a held port was freed writes the same
// file and still has to reach the server.
func (n *NativeService) reloadServer(ctx context.Context) SaveAck {
	if _, running := n.serverLock(); !running {
		return SaveAck{Outcome: "not_running", RestartNeeded: []string{}}
	}
	home := filepath.Dir(n.configPath)
	cur, err := n.requestReload(ctx, home, n.reloadWait)
	ack := SaveAck{
		Generation:    cur.Generation,
		HTTPURL:       cur.HTTP,
		TLSListening:  cur.TLS,
		RestartNeeded: []string{},
	}
	if cur.RestartNeeded != nil {
		ack.RestartNeeded = cur.RestartNeeded
	}
	if l, ok := cur.Listeners["http"]; ok && l.Reason != "off" {
		ack.HTTPReason = l.Reason
	}
	if l, ok := cur.Listeners["tls"]; ok && l.Reason != "off" {
		ack.TLSReason = l.Reason
	}
	var (
		failed  *serveraddr.ReloadListenerError
		refused *serveraddr.ReloadRefusedError
		timeout *serveraddr.ReloadTimeoutError
	)
	switch {
	case err == nil && len(ack.RestartNeeded) > 0:
		ack.Outcome = "restart_needed"
	case err == nil:
		ack.Outcome = "applied"
	case errors.As(err, &failed):
		ack.Outcome = "listener_failed"
		ack.Error = err.Error()
	case errors.As(err, &refused):
		ack.Outcome = "refused"
		ack.Error = refused.Message
	case errors.As(err, &timeout):
		ack.Outcome = "timeout"
		ack.Error = err.Error()
	default:
		ack.Outcome = "unavailable"
		ack.Error = err.Error()
	}
	if ack.Outcome != "applied" && ack.Outcome != "restart_needed" {
		log.Warn().Str("outcome", ack.Outcome).Str("error", ack.Error).Msg("settings saved; the server did not apply them as a whole")
	}
	return ack
}

// RestartServer restarts the knomit SERVER the desktop spawned — not the app:
// the desktop keeps running and its window reconnects once the replacement
// answers. It returns when the new server answers, or with the reason it did
// not (including after restartWait). Refused for a server started outside the
// app (attach mode: it is not ours to stop), named "started outside the app
// (pid N)".
//
// Gated to the Settings window, like everything that changes what the server
// listens on (kb invariant desktop/fleet/install-surface): a restart applies
// a key a reload could not.
func (n *NativeService) RestartServer(ctx context.Context) error {
	if !callerIsSettings(ctx) {
		return errNotSettingsWindow
	}
	if n.restartServer == nil {
		return errors.New("no server to restart")
	}
	if n.restartWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, n.restartWait)
		defer cancel()
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
