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
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	knomitapp "knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/platform/version"
	webui "knomit/web"

	desktopui "knomit/tools/desktop/ui"

	"knomit/tools/desktop/internal/autostart"
	"knomit/tools/desktop/internal/lockfile"
	"knomit/tools/desktop/internal/paths"
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

// wailsOrigins are the page origins Wails serves assets from, by platform
// (confirmed in the Task 0 spike: darwin/linux use the "wails" scheme, windows
// uses http). Both are allowed for CORS since the API binds looknomitck-only.
var wailsOrigins = []string{"wails://localhost", "http://wails.localhost"}

// desktopAppOptions are the app options the desktop boots with.
func desktopAppOptions() knomitapp.Options {
	return knomitapp.Options{
		APIOnly:     true,
		CORSOrigins: wailsOrigins,
		// The desktop never opens the OAuth listener, so it builds no
		// issuer: the pending endpoints are 404 and the web UI hides its
		// panel (F19 phase 3b, W3).
		NoOAuth: true,
		// The Logs tab streams from this, through the very API base
		// configInjectingHandler injects below. Serving it from the in-process
		// server is what keeps the log stream part of the API rather than a
		// second, desktop-only transport.
		LogTap: logTap,
	}
}

// run boots the in-process server and the Wails desktop shell.
func run(ctx context.Context) error {
	lockPath, err := paths.LockfilePath()
	if err != nil {
		return err
	}
	if err := singleinstance.Acquire(lockPath); err != nil {
		if errors.Is(err, singleinstance.ErrAlreadyRunning) {
			fmt.Println("knomit-desktop is already running.")
			return nil
		}
		// A real error checking the lockfile (e.g. unreadable) — surface it
		// rather than masking it as "already running".
		return fmt.Errorf("check single instance: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// The log file, resolved ONCE. Three things have to agree on it — the
	// logger's own sink, "Reveal in Finder", and the path the Settings dialog
	// shows — and resolving it separately per consumer is what let a configured
	// `[log] file` send the log somewhere the rest of the app was not looking.
	// See resolveLogFile.
	logFile := resolveLogFile(cfg)

	// Phase two of logging: now that knomit.toml has been read, rebuild the
	// logger so the user's level and format apply. Phase one (bootstrapLogging)
	// is what caught any failure in config.Load above.
	if lerr := applyLogConfig(cfg.Log, logFile); lerr != nil {
		log.Warn().Err(lerr).Msg("log config not applied; keeping bootstrap logger")
	}
	// Says out loud which file everything downstream agreed on. main.go already
	// logs the BOOTSTRAP path, and the two differ whenever knomit.toml names a
	// file — so without this line the log's own account of where it lives is the
	// path it stopped using seconds earlier. It is also the only externally
	// visible evidence that Reveal and the Settings dialog will name the same
	// file the logger is writing.
	if logFile == "" {
		log.Warn().Msg("no log file could be resolved; logging to stderr only, so there is no post-mortem record on disk")
	} else {
		log.Info().Str("log_file", logFile).Msg("logging to file")
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

	// Start the server FIRST but do not wait for it. Everything below this line
	// is cheap; bootKnomit is not (seconds, dominated by loading the embedder
	// and populating each repo's commit log). Running it inline is what used to
	// keep the tray icon off the menu bar until it finished. Now it overlaps
	// Wails' own startup, and the tray appears wearing the amber boot badge.
	// The TLS listener's state, for Settings' Fleet identity section: made
	// here because the boot starts before NativeService exists (see below).
	tlsSt := &tlsStatus{}
	boot := startServerBoot(ctx, func(ctx context.Context, setPhase func(bootPhase)) (string, func(), error) {
		return bootKnomit(ctx, cfg, lockPath, setPhase, tlsSt)
	})
	// Wails calls os.Exit on quit, so Go defers in run() do not fire. Cleanup
	// runs via Wails' OnShutdown hook instead; serverBoot.stop is idempotent, so
	// the belt-and-braces call after Run() below is harmless.
	shutdown := boot.stop

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
	// only. configPath is the file config.findConfigFile falls through to, which
	// on a bundle is the only one there is.
	nativeSvc := newNativeService(
		filepath.Join(cfg.Home, "knomit.toml"), logFile, autostart.New())
	nativeSvc.tls = tlsSt
	// Restarting must release this process's single-instance lockfile before
	// spawning the replacement — see the releaseInstance field comment on
	// NativeService. boot.stop is the same idempotent teardown OnShutdown
	// below calls, so running it early here and again on the eventual
	// shutdown is safe. lockfile.Remove is called explicitly too, rather than
	// relying solely on stop()'s internal removal: stop() gives up WITHOUT
	// calling its teardown (and so without removing the lockfile) if a boot
	// still in flight has not settled within stopGrace — reachable here
	// precisely because Settings, and so Restart, is available before boot
	// completes. Safe only as long as stop()'s sync.Once means the later
	// OnShutdown-driven call is always a no-op after this one — see the
	// releaseInstance field comment for the risk if that ever changes.
	nativeSvc.releaseInstance = func() {
		boot.stop()
		_ = lockfile.Remove(lockPath)
	}
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
			Handler: configInjectingHandler(uiFS, desktopFS, boot.status),
		},
		Services:   services,
		OnShutdown: shutdown,
	})
	// Quit this instance only AFTER RestartApp has released the lockfile and
	// spawned the replacement — see NativeService.RestartApp.
	nativeSvc.onRestart = wapp.Quit

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
				announcer, seen, wapp.Updater, updCfg.CurrentVersion)
		})

		// Best-effort: a broken update channel must never stop the app starting.
		// Hide the item rather than leave it: with no updater behind it, a
		// "Check for Updates…" that silently does nothing is worse than absent.
		start, cerr := configureUpdater(ctx, wapp, notifySvc, announcer, seen, updCfg)
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

	// Clear the amber boot badge once the server answers. A boot FAILURE is
	// fatal — the tray would otherwise sit there looking installed while every
	// window it opens is dead — so say why in a dialog and quit. Before the
	// server moved off the main path this was a plain error return from run();
	// the dialog is what replaces the message on a stderr nobody sees, because
	// LaunchServices points a bundle's stderr at /dev/null.
	go func() {
		apiBase, err := boot.wait(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // quitting already; the shutdown path has it
			}
			log.Error().Err(err).Msg("knomit-desktop: server failed to start")
			// Through the gate, because a FAST failure (an unwritable
			// cfg.Home, or bootServer failing to listen) can land here before
			// Run() has assigned App.impl — and both calls below dereference
			// it. Dialog.Error().Show() reaches impl.isOnMainThread() through
			// InvokeSync and panics on the nil interface, on a non-main
			// goroutine, with a bundle's stderr at /dev/null: the user sees
			// nothing at all. Quit() is quieter and no better — it is a
			// documented no-op while impl is nil, which would leave the app
			// sitting there with a dead server and a permanently amber tray.
			//
			// (SystemTray.SetIcon, the other pre-Run Wails call in this file,
			// IS nil-guarded upstream — which is exactly why the tray's
			// synchronous apply needs no gate and this does.)
			gate.after(func() {
				wapp.Dialog.Error().
					SetTitle("Knomit could not start").
					SetMessage(err.Error()).
					Show()
				wapp.Quit()
			})
			return
		}
		// The bound port is only knowable here — it can differ from the
		// configured one when 19278 was taken. Published BEFORE the boot badge
		// clears, so nothing that reacts to "booted" can observe a zero port.
		// Neither call touches Wails' platform layer (setEffectivePort is our
		// own mutex; SetIcon is nil-guarded upstream), so neither is gated.
		nativeSvc.setEffectivePort(portFromBaseURL(apiBase))
		trayIcon.setBooting(false)
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
	// so deferred shutdown (lockfile removal, app close) runs on a clean exit.
	go func() {
		<-ctx.Done()
		wapp.Quit()
	}()

	// The gap between this line and "server up (API-only)" is the window the
	// boot badge covers. It used to be the other way round — the tray could not
	// appear until the server had, which is the whole reason bootKnomit moved
	// off this path — so log it, or a regression to that ordering is invisible.
	log.Info().Msg("knomit-desktop UI starting (tray up, server still booting)")

	err = wapp.Run()
	shutdown() // in case Run returns without triggering OnShutdown
	return err
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

// bootKnomit starts everything behind the UI: the bundled CLI links, the
// in-process knomit application, and the HTTP server it is served on. It
// returns the API base URL and a teardown for what it started.
//
// This is the slow half of startup and it runs on a goroutine (see
// startServerBoot), so it must not touch Wails — nothing here does.
func bootKnomit(ctx context.Context, cfg config.Config, lockPath string, setPhase func(bootPhase), tlsSt *tlsStatus) (string, func(), error) {
	setPhase(phaseInstallingTools)
	// Expose the bundled knomit-bridge at a stable path so stdio MCP clients
	// (Claude Code/Desktop, VS Code) can launch it regardless of where the app
	// lives. Best-effort: a failure must not block the app from starting.
	if link, lerr := installBridgeTool(cfg.Home); lerr != nil {
		log.Warn().Err(lerr).Msg("knomit-bridge: MCP integration link not installed")
	} else {
		log.Info().Str("path", link).Msg("knomit-bridge available for MCP clients")
	}

	// Same for knomit-okf, so the OKF export CLI is runnable by name rather
	// than by a path inside the app bundle. Also best-effort.
	if link, lerr := installOKFTool(cfg.Home); lerr != nil {
		log.Warn().Err(lerr).Msg("knomit-okf: CLI link not installed")
	} else {
		log.Info().Str("path", link).Msg("knomit-okf available on the command line")
	}

	// Which phase app.New is about to spend its time in. On a FIRST launch it is
	// dominated by fetching ~640 MB of model artifacts; on every launch after
	// that those are on disk and the same call is seconds of loading weights and
	// opening repos. Saying "Downloading models…" in the second case would be a
	// label that lies on all but one launch in the app's life, so ask before
	// claiming it. embeddings.ModelCached is presence-only and cheap (three
	// os.Stat calls); a lookup failure is not fatal here, since the phase is a
	// caption and app.New will report the real error moments later.
	downloading := false
	if m, lerr := embeddings.Lookup(cfg.Embeddings.Model); lerr == nil {
		downloading = !embeddings.ModelCached(m, filepath.Join(cfg.Home, "models"))
	}
	if downloading {
		setPhase(phaseDownloadingModels)
	} else {
		setPhase(phaseStartingEngine)
	}

	// In-process server: API/MCP/git only (no UI), CORS for the Wails origin.
	a, err := knomitapp.New(ctx, cfg, desktopAppOptions())
	if err != nil {
		return "", nil, err
	}

	setPhase(phaseStartingServer)
	srv, port, err := bootServer(ctx, a.Handler(), lockPath, version.String(), cfg, a.KeyPath())
	if err != nil {
		a.Close()
		return "", nil, err
	}
	tlsSt.set(srv.tlsState)
	apiBase := fmt.Sprintf("http://127.0.0.1:%d", port)
	log.Info().Str("api", apiBase).Int("port", port).Str("socket", cfg.Socket).Msg("knomit-desktop server up (API-only)")
	return apiBase, func() { srv.shutdown(); a.Close() }, nil
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
// a base when there is one, __KNOMIT_BOOTING__ when there is not. The client
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
			// Never cache: the API base embeds the chosen port, which can differ
			// between launches (ephemeral fallback when 19278 is taken). A cached
			// copy would point the UI at a dead port — and while booting there is
			// no base at all, which is the last thing to let a cache remember.
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprint(w, "window.__KNOMIT_DESKTOP__ = true;\n")
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
