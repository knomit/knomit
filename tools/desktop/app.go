//go:build desktop

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"knomit/internal/domain/modelparams"
	"knomit/internal/foundation/config"
	"knomit/internal/platform/privdir"
	"knomit/internal/platform/version"
	webui "knomit/web"

	desktopui "knomit/tools/desktop/ui"

	"knomit/tools/desktop/internal/autostart"
	"knomit/tools/desktop/internal/singleinstance"
)

// appIcon is the colored knomit logo used as the application/window icon
// (rendered from web/public/logo.svg; see `make desktop-icons`). On Linux it
// is the only source for the window/taskbar/alt-tab icon — Wails derives those
// from Options.Icon, and the Linux binary has no .app bundle to fall back on.
// On macOS the bundle's icon.icns drives the Dock icon; this is just the
// about-box icon there. The per-platform tray icon lives in trayicon_*.go.
//
//go:embed appicon.png
var appIcon []byte

// desktopLockPath is the desktop's single-instance lock for the data root
// home: desktop.lock beside that home's server.json, i.e. <StateDir>/desktop.lock
// for the default home and <home>/desktop.lock for any other. Keyed by home
// like server.json, so a desktop on a scratch KNOMIT_HOME can run beside the
// user's (kb gotcha desktop/rebuilding-kills-the-running-server). server.json
// itself is the SERVER's file and no longer doubles as the desktop's lock.
func desktopLockPath(home string) string {
	return filepath.Join(filepath.Dir(config.LockfilePathFor(home)), "desktop.lock")
}

// run hosts the UI over the knomit server's local socket / pipe, attaching to a
// server that already answers there or supervising one it spawns (see
// supervisor.go). The desktop contains no server of its own.
func run(ctx context.Context) error {
	home, err := config.ResolveHome()
	if err != nil {
		return err
	}
	// Synchronous, before any UI: a second instance must fail before it builds
	// anything (kb invariant desktop/startup/wails). The OS releases the lock
	// when this process ends, however it ends.
	lock, err := singleinstance.Acquire(desktopLockPath(home))
	if err != nil {
		if errors.Is(err, singleinstance.ErrAlreadyRunning) {
			fmt.Println("knomit-desktop is already running.")
			return nil
		}
		return fmt.Errorf("check single instance: %w", err)
	}
	defer lock.Release()

	// A fresh install starts with the plaintext HTTP port off (socket only);
	// an upgrade keeps it. Decided here, before any server is spawned and
	// before the config is read, so the first boot already reads it.
	if wrote, ferr := ensureFreshInstallDefaults(home); ferr != nil {
		log.Warn().Err(ferr).Msg("fresh-install defaults not written; the server will start with the HTTP port on")
	} else if wrote {
		log.Info().Msg("fresh install: wrote knomit.toml with http = false (socket only); Settings turns the HTTP port on")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// The server's discovery file for this home: read to name an attached
	// server's pid and, by Settings, its listeners.
	lockPath := config.LockfilePathFor(cfg.Home)

	// Two log files, resolved ONCE. The SERVER owns the configured one (it is
	// passed --log-file), so Reveal and the Settings dialog name the file being
	// written; the desktop shell writes its own knomit-desktop.log beside it.
	// Two processes must never rotate one file.
	serverLog := resolveLogFile(cfg)
	desktopLog := desktopLogFile(serverLog)

	// Phase two of logging: now that knomit.toml has been read, rebuild the
	// logger so the user's level and format apply. Phase one (bootstrapLogging)
	// is what caught any failure in config.Load above.
	if lerr := applyLogConfig(cfg.Log, desktopLog); lerr != nil {
		log.Warn().Err(lerr).Msg("log config not applied; keeping bootstrap logger")
	}
	if ignored := config.IgnoredExecutableConfig(cfg.Home); ignored != "" {
		log.Warn().Str("ignored", ignored).Str("read", filepath.Join(cfg.Home, "knomit.toml")).
			Msg("knomit.toml beside the executable is not read; move its settings into the data root's knomit.toml")
	}
	// Says out loud which files everything downstream agreed on: main.go logs
	// only the bootstrap path.
	if serverLog == "" {
		log.Warn().Msg("no log file could be resolved; the server logs to stderr only, so there is no post-mortem record on disk")
	} else {
		log.Info().Str("server_log", serverLog).Str("desktop_log", desktopLog).Msg("logging to file")
	}

	uiFS, err := webui.FS()
	if err != nil {
		return fmt.Errorf("embedded UI: %w", err)
	}

	// The desktop-only bundle (Settings, Logs), served under /desktop/. Kept out
	// of webui because that tree is embedded in the server binary too.
	desktopFS, err := desktopui.FS()
	if err != nil {
		return fmt.Errorf("embedded desktop UI: %w", err)
	}

	// Status changes reach the tray and the fatal dialog through this one-slot
	// signal: the supervisor never blocks on the UI, and the consumer (started
	// once the tray exists) always reads the CURRENT status, so a burst of
	// transitions cannot be applied out of order.
	changed := make(chan struct{}, 1)
	// Where the webview's API traffic goes once a server answers. See uiproxy.go.
	uiUp := &uiUpstream{}
	sup := newSupervisor(supervisorConfig{
		socket:         cfg.Socket,
		home:           cfg.Home,
		lockPath:       lockPath,
		logFile:        serverLog,
		desktopVersion: version.String(),
		serverBin:      serverBinaryPath,
		environ:        os.Environ,
		installTools:   func() { installDesktopTools(cfg.Home) },
		modelsCached:   func() bool { return modelsCached(cfg) },
		up:             uiUp,
		onChange: func(bootStatus) {
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	})
	// Started FIRST but not waited for. Everything below this line is cheap;
	// the server's boot is not (seconds; minutes on a first launch while models
	// download), and it now happens in another process anyway. The tray appears
	// wearing the amber boot badge.
	sup.start(ctx)
	uiStatus := uiStatusFrom(sup)
	// Wails calls os.Exit on quit, so Go defers in run() do not fire. Cleanup
	// runs via Wails' OnShutdown hook instead; sup.stop is idempotent, so the
	// belt-and-braces call after Run() below is harmless.
	shutdown := sup.stop

	// Self-update (macOS only — see updaterConfig). Resolved BEFORE the app is
	// built, because the notifications service is registered only where
	// updates can actually run: a Linux or dev build should not take on a
	// notification-daemon dependency for a feature it does not have.
	updCfg, updatesEnabled, uerr := updaterConfig(runtime.GOOS)
	switch {
	case uerr != nil:
		// A misconfigured key, not a deliberate opt-out — the error IS the
		// reason, so don't follow it with selfUpdateDisabledReason's guesses.
		log.Warn().Err(uerr).Msg("self-update unavailable")
		updatesEnabled = false
	case !updatesEnabled:
		log.Info().Str("reason", selfUpdateDisabledReason()).Msg("self-update disabled")
	}

	// The Settings dialog reads and writes through this service, over Wails IPC
	// only. configPath is <cfg.Home>/knomit.toml, the only file config.Load
	// reads.
	nativeSvc := newNativeService(
		filepath.Join(cfg.Home, "knomit.toml"), serverLog, autostart.New())
	nativeSvc.restartServer = sup.restartServer
	nativeSvc.isAttached = sup.isAttached
	nativeSvc.desktopVersion = version.String()
	services := []application.Service{application.NewService(nativeSvc)}
	var notifySvc *notifications.NotificationService
	if updatesEnabled {
		notifySvc = notifications.New()
		services = append(services, application.NewService(notifySvc))
	}

	wapp := application.New(application.Options{
		Name: "Knomit",
		Icon: appIcon,
		Assets: application.AssetOptions{
			Handler: apiProxy(configInjectingHandler(uiFS, desktopFS, uiStatus),
				func() bool { return sup.status().Ready }, uiUp),
		},
		Services:   services,
		OnShutdown: shutdown,
	})
	registerSSEStream(wapp, uiUp)
	// An install-and-restart replaces the whole .app, the bundled server
	// included, and relaunches: the server this desktop spawned must be gone
	// first, or it keeps running the replaced binary (kb gotcha
	// desktop/self-update, 7173001e).
	installer := stopServerFirst{updateInstaller: wapp.Updater, stop: sup.stop, failed: sup.fail}

	window := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Knomit",
		Width:  1200,
		Height: 800,
		// Knomit is a tray app: the menu bar is the entry point, and the window
		// opens only when "Open Knomit" is chosen. Without this Wails shows it
		// on every launch, including the login-item launch that "Start at
		// login" schedules — a window in your face at every boot.
		Hidden: true,
		// Hide the native title bar (no duplicate "Knomit"): the web app's own
		// header becomes the top of the window. Traffic-light controls remain;
		// the frontend insets its header on desktop and makes it draggable.
		Mac: application.MacWindow{
			TitleBar: application.MacTitleBarHidden,
		},
	})
	window.SetURL("/")
	// Export asks where to save: the native save dialog, attached to the main
	// window. The page only suggests a name; the user picks the path.
	nativeSvc.pickSavePath = func(dir, suggested string) (string, error) {
		d := wapp.Dialog.SaveFile().
			AttachToWindow(window).
			SetFilename(suggested)
		if dir != "" {
			d = d.SetDirectory(dir)
		}
		return d.CanCreateDirectories(true).PromptForSingleSelection()
	}

	// Hide (don't destroy) the window when closed, so "Open Knomit" can bring
	// it back. Registered as a hook so Cancel() short-circuits Wails' default
	// destroy listener.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		window.Hide()
	})

	tray := wapp.SystemTray.New()
	trayIcon := newTrayIconState(wapp, tray)
	menu := wapp.NewMenu()
	menu.Add("Open Knomit").OnClick(func(_ *application.Context) {
		window.Show()
		window.Focus()
	})
	// The desktop-only window. Lazy — no webview is built until the user asks
	// for one. See windows.go.
	//
	// There is no "Logs…" item any more: the log lives in the main window, at
	// Manage → Logs, streamed over the API. A tray item cannot deep link there
	// because opening Manage is a click rather than URL state (App.tsx says so
	// deliberately), and inventing a boot-time parameter to carry it would be a
	// larger change than the two clicks it saves.
	aux := newAuxWindows(wapp)
	menu.Add("Settings…").OnClick(func(_ *application.Context) { aux.ShowSettings() })
	// Only where self-update is live. On Linux (AppImage, no self-update) and
	// in dev builds this would be a button that does nothing, which is worse
	// than no button.
	//
	// startUpdates is the half of the update wiring that must not run before
	// the application is up — see configureUpdater. It stays nil when
	// self-update is off or failed to configure, and the ApplicationStarted
	// hook below is registered only when it is not.
	var startUpdates func()
	if updatesEnabled {
		// The item is both surfaces at once: a check button until a release is
		// found, the install button after. Notification delivery is
		// best-effort on an unsigned bundle, so this is the update path that
		// does not depend on the notification centre accepting anything.
		item := menu.Add("Check for Updates…")
		pending := &pendingUpdate{}
		announcer := &trayUpdateAnnouncer{
			pending: pending,
			icon:    trayIcon,
			item:    item,
			menu:    menu,
			tray:    tray,
		}
		// Which version the user has already been shown a banner for, read
		// from <StateDir>/update.json. Held across launches so an update left
		// unclaimed does not re-announce itself on every start.
		seen := newNotifyLog()
		item.OnClick(func(_ *application.Context) {
			updateMenuAction(ctx, pending, wapp.Updater, notifySvc,
				announcer, seen, installer, updCfg.CurrentVersion)
		})

		// Best-effort: a broken update channel must never stop the app starting.
		// Hide the item rather than leave it: with no updater behind it, a
		// "Check for Updates…" that silently does nothing is worse than absent.
		start, cerr := configureUpdater(ctx, wapp, installer, notifySvc, announcer, seen, updCfg)
		if cerr != nil {
			log.Warn().Err(cerr).Msg("self-update unavailable")
			item.SetHidden(true)
		} else {
			startUpdates = start
		}
	}
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(_ *application.Context) { wapp.Quit() })
	tray.SetMenu(menu)

	// Nothing below may touch Wails before Run() has built the platform app —
	// see appStartGate. ApplicationStarted is applicationDidFinishLaunching on
	// macOS, ApplicationStartup on Linux and Windows, and in all three it fires
	// from inside Run once App.impl exists.
	gate := &appStartGate{}
	wapp.Event.OnApplicationEvent(events.Common.ApplicationStarted,
		func(*application.ApplicationEvent) { gate.open() })

	// The tray badge follows the server: amber while none answers (first boot,
	// or a crashed server being restarted), cleared while one does. A FAILED
	// supervisor (exit 2, the restart budget spent, an incomplete bundle) is
	// fatal — the tray would otherwise sit there looking installed while every
	// window it opens is dead — so say why in a dialog and quit. LaunchServices
	// points a bundle's stderr at /dev/null, so the dialog is the only place
	// the user can read it.
	go func() {
		for {
			st := sup.status()
			switch {
			case st.Phase == string(phaseFailed):
				if ctx.Err() != nil {
					return // quitting already; the shutdown path has it
				}
				msg := st.Error
				// Through the gate, because a FAST failure (an incomplete
				// bundle) can land here before Run() has assigned App.impl —
				// and both calls below dereference it. Dialog.Error().Show()
				// reaches impl.isOnMainThread() through InvokeSync and panics
				// on the nil interface, on a non-main goroutine, with a
				// bundle's stderr at /dev/null: the user sees nothing at all.
				// Quit() is a documented no-op while impl is nil, which would
				// leave the app sitting there with no server and a permanently
				// amber tray. (SystemTray.SetIcon, used below, IS nil-guarded
				// upstream, which is why the badge needs no gate.)
				gate.after(func() {
					wapp.Dialog.Error().
						SetTitle("Knomit could not start").
						SetMessage(msg).
						Show()
					wapp.Quit()
				})
				return
			case st.Ready:
				trayIcon.setBooting(false)
			default:
				trayIcon.setBooting(true)
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
		}
	}()

	// Everything that touches the OS notification centre starts HERE, not
	// above: ApplicationStarted is the first point at which NSApplication
	// exists and Wails has run the notifications service's own startup guards
	// — reaching UNUserNotificationCenter before that raises rather than
	// returns. See configureUpdater.
	if startUpdates != nil {
		gate.after(startUpdates)
	}

	// Quit the Wails run loop when the context is cancelled (SIGINT/SIGTERM),
	// so the OnShutdown teardown (stopping a spawned server) runs.
	go func() {
		<-ctx.Done()
		wapp.Quit()
	}()

	// The gap between this line and "knomit server is answering" is the window
	// the boot badge covers. The tray must come first; if this line ever
	// follows the server's readiness, startup has regressed to waiting on it.
	log.Info().Msg("knomit-desktop UI starting (tray up, server still booting)")

	err = wapp.Run()
	shutdown() // in case Run returns without triggering OnShutdown
	return err
}

// uiStatusFrom is the status the UI sees: the supervisor's, with the API base
// set to the page's OWN origin once a server answers, because the webview
// reaches the API through the asset handler's /api/ proxy (uiproxy.go).
func uiStatusFrom(sup *supervisor) func() bootStatus {
	return func() bootStatus {
		s := sup.status()
		if s.Ready {
			s.APIBase = desktopOrigin()
		}
		return s
	}
}

// installDesktopTools is the desktop's own install step, the installing-tools
// phase: the bundled kb and knomit-okf linked into <home>/bin so MCP clients
// and the shell can run them by a stable path. Best-effort throughout: a
// failure is logged and the server is started anyway.
func installDesktopTools(home string) {
	// The data root is private before installBridgeTool creates <home>/bin
	// (at 0755), which on a first launch creates <home> itself. The server
	// runs the same check as its backstop.
	if err := privdir.Ensure(home); err != nil {
		log.Warn().Err(err).Str("home", home).Msg("data root is not private; the server will report it")
		return
	}
	// Expose the bundled knomit-bridge at a stable path so stdio MCP clients
	// (Claude Code/Desktop, VS Code) can launch it regardless of where the app
	// lives.
	if link, lerr := installBridgeTool(home); lerr != nil {
		log.Warn().Err(lerr).Msg("knomit-bridge: MCP integration link not installed")
	} else {
		log.Info().Str("path", link).Msg("knomit-bridge available for MCP clients")
	}
	// Same for knomit-okf, so the OKF export CLI is runnable by name rather
	// than by a path inside the app bundle.
	if link, lerr := installOKFTool(home); lerr != nil {
		log.Warn().Err(lerr).Msg("knomit-okf: CLI link not installed")
	} else {
		log.Info().Str("path", link).Msg("knomit-okf available on the command line")
	}
}

// modelsCached reports whether the server's next start will find its
// embedding model on disk. It only picks the boot screen's caption: on a FIRST
// launch the server spends minutes fetching ~640 MB of model artifacts, on
// every later one the same step is seconds of loading weights, and "Downloading
// models…" would be a label that lies on all but one launch in the app's life.
// params.ModelCached is presence-only and cheap (and cgo-free: the desktop does
// not link the ONNX runtime or the tokenizers library for a caption); a lookup failure reads as
// cached, since the caption is not worth an error and the server reports the
// real one.
func modelsCached(cfg config.Config) bool {
	m, err := modelparams.Lookup(cfg.Embeddings.Model)
	if err != nil {
		return true
	}
	return modelparams.ModelCached(m, filepath.Join(cfg.Home, "models"))
}

// appStartGate defers work that must not reach Wails until the application has
// actually started.
//
// The hazard it closes is a lifetime one, not a threading one.
// application.App.impl — the platform application every Dialog, Quit and
// InvokeSync call dereferences — is assigned INSIDE Run(). Everything run()
// builds beforehand (the tray, the menu, the boot watcher goroutine) exists
// before that assignment, so a goroutine that outraces Run() to a Wails call
// finds a nil interface and panics where nobody can see it. Registering the
// call here instead means it runs at ApplicationStarted, or immediately if that
// has already fired.
//
// Deliberately not "check a bool and call": the check and the call have to be
// atomic with respect to open(), or a failure arriving in that gap is queued
// onto a list that has already been drained and never runs at all.
type appStartGate struct {
	mu      sync.Mutex
	started bool
	pending []func()
}

// after runs fn once the application has started — now, if it already has.
// Callable from any goroutine.
func (g *appStartGate) after(fn func()) {
	g.mu.Lock()
	if !g.started {
		g.pending = append(g.pending, fn)
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	fn()
}

// open marks the application started and drains what was waiting, in the order
// it was registered. Idempotent — a second ApplicationStarted (or a test
// calling it twice) must not re-run anything.
func (g *appStartGate) open() {
	g.mu.Lock()
	g.started = true
	pending := g.pending
	g.pending = nil
	g.mu.Unlock()
	// Outside the lock: these call into Wails, and one of them quits the app.
	for _, fn := range pending {
		fn()
	}
}

// bootStatusPath is the endpoint the boot screen polls while the server is
// still coming up. It is served by THIS handler, in the desktop process, which
// is the whole point: during a first launch the knomit server does not exist
// yet, so the only thing that can answer is the process that is booting it.
const bootStatusPath = "/boot/status"

// desktopPrefix is where the desktop-only UI bundle (Settings, Logs) lives.
// Everything outside it belongs to the shared knowledge app, which owns the
// root and its SPA fallback.
const desktopPrefix = "/desktop/"

// wailsPrefix is the framework's reserved path space (/wails/runtime and
// /wails/runtime.js). Wails answers it in middleware wrapping this handler;
// see the guard in configInjectingHandler for why we refuse it anyway.
const wailsPrefix = "/wails/"

// configInjectingHandler serves /config.js with the live API base, answers
// GET /boot/status while the server is still booting, serves the embedded UI
// assets, falls back to index.html for client-side routes, and serves the
// desktop-only bundle under /desktop/.
//
// status is non-blocking and is asked PER REQUEST, never captured: the answer
// changes underneath this handler as the boot progresses, which is the only
// reason /boot/status can say anything useful.
//
// /config.js USED TO BLOCK until the server was up, on the reasoning that the
// webview should take a moment longer to paint rather than load against an
// address that does not exist. That is a good trade for a two-second boot and a
// bad one for a first launch, which spends MINUTES fetching model artifacts:
// index.html loads config.js with a plain <script> tag, so blocking it blocks
// the document, and the user watches a static splash with no way to learn what
// is happening. Worse, the wait was capped — past the cap config.js 503'd, the
// page loaded with no API base at all, and every later request resolved against
// the webview origin instead, where the SPA fallback below takes it.
//
// What that fallback does to an API path is worth stating exactly, because the
// obvious guess ("it answers 200 with index.html") is wrong and leads to the
// wrong fix. Measured against this handler:
//
//	/                     200, the app
//	/toplevel             301 -> "./" -> "/" -> 200 in one hop
//	/api/v1/repos         301 -> "./" -> "/api/v1/" -> 301 -> ... a LOOP
//
// The rewrite to /index.html hands the request to http.FileServer, which 301s
// any path ending in index.html to "./" — and "./" resolves against the REQUEST
// path, so a NESTED path lands on its own parent and does it again. A Go client
// gives up after 10 hops; a browser reports a redirect error. Every /api/v1/...
// path is nested, so every API call taken by this fallback dies that way. See
// kb/gotchas/web/spa/fileserver-index-redirect-loop, which is the same trap the
// server-side handler already fixed with http.ServeContent.
//
// Either way the UI cannot tell it from a server that is merely unwell, so it
// retried forever and never recovered, even once the server was up.
//
// So it answers immediately, and says which of the two worlds the client is in:
// a base when there is one (the page's own origin: /api/ is proxied to the
// server's local socket, see apiProxy), __KNOMIT_BOOTING__ when there is not. The client
// polls /boot/status and picks the base up from there (see web/src/bootStatus.ts).
//
// desktopFS may be nil, which disables the /desktop/ tree entirely. run always
// passes a real one; the nil case is what lets a test exercise the shared-UI
// half without standing up a second filesystem.
func configInjectingHandler(uiFS, desktopFS fs.FS, status func() bootStatus) http.Handler {
	fileServer := http.FileServer(http.FS(uiFS))
	var desktopServer http.Handler
	if desktopFS != nil {
		desktopServer = http.StripPrefix(desktopPrefix, http.FileServer(http.FS(desktopFS)))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Desktop-only UI (Settings, Logs). Checked before everything below,
		// which would otherwise swallow these paths into index.html and leave
		// the window showing the knowledge app — or, worse, nothing at all.
		if desktopServer != nil && strings.HasPrefix(r.URL.Path, desktopPrefix) {
			desktopServer.ServeHTTP(w, r)
			return
		}
		// Wails' own endpoints (/wails/runtime, /wails/runtime.js) are answered
		// by framework middleware that wraps this handler, so nothing under
		// /wails/ should ever arrive here. If ordering ever changes so that it
		// does, the SPA fallback below would answer a runtime call with
		// index.html and a 200 — and the frontend, seeing an OK response, would
		// believe the call succeeded. External links open through
		// Browser.OpenURL over that endpoint (see web/src/externalLinks.ts);
		// that failure would put them right back to doing nothing on click.
		// 404 instead, so the breakage is visible.
		if strings.HasPrefix(r.URL.Path, wailsPrefix) {
			http.NotFound(w, r)
			return
		}
		// The boot screen's only source of truth until the API exists. Always
		// 200, including for a FAILED boot: the failure is data the boot screen
		// renders, not a transport error, and an HTTP error status here would
		// just make the client guess at what went wrong.
		if r.URL.Path == bootStatusPath {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if err := json.NewEncoder(w).Encode(status()); err != nil {
				log.Warn().Err(err).Msg("boot status: encode failed")
			}
			return
		}
		if r.URL.Path == "/config.js" {
			st := status()
			w.Header().Set("Content-Type", "application/javascript")
			// Never cache: while booting there is no base at all, which is the
			// last thing to let a cache remember, and the transport flags must
			// reflect this launch.
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprint(w, "window.__KNOMIT_DESKTOP__ = true;\n")
			// The UI opens its event streams as Wails Streams rather than
			// EventSource (web/src/eventStream.ts, uistreams.go). Unconditional,
			// not gated on the boot: the flag does not depend on the server, and
			// the boot gate keeps every stream closed until it is up, so a page
			// that loaded while booting still finds it set when it first opens one.
			fmt.Fprint(w, "window.__KNOMIT_EVENT_TRANSPORT__ = \"wails-stream\";\n")
			if !st.Ready {
				// Deliberately no __KNOMIT_API_BASE__. Leaving it UNSET is what
				// makes the client's gate unmissable: any code that skips the
				// boot gate and calls the API anyway resolves against the
				// webview origin, and the SPA fallback answers HTML — so the
				// flag has to be the thing consulted, not the base's emptiness.
				fmt.Fprint(w, "window.__KNOMIT_BOOTING__ = true;\n")
				return
			}
			fmt.Fprintf(w, "window.__KNOMIT_API_BASE__ = %q;\n", st.APIBase)
			return
		}
		// SPA fallback: serve index.html when the path is not a real asset.
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, statErr := fs.Stat(uiFS, p); statErr != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/index.html"
		}
		fileServer.ServeHTTP(w, r)
	})
}
