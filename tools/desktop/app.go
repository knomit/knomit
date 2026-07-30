//go:build desktop

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	knomitapp "knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/version"
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

	// The log file, resolved ONCE. Four things have to agree on it — the
	// logger's own sink, the Logs window's tailer, "Reveal in Finder", and the
	// path the Settings dialog shows — and resolving it separately per consumer
	// is what let a configured `[log] file` produce a permanently blank Logs
	// window. See resolveLogFile.
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
	// visible evidence that the Logs window and the logger resolved the same
	// file, short of opening the window.
	if logFile == "" {
		log.Warn().Msg("no log file could be resolved; logging to stderr only, and the Logs window will have nothing to show")
	} else {
		log.Info().Str("log_file", logFile).Msg("logging to file; the Logs window follows this path")
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
	boot := startServerBoot(ctx, func(ctx context.Context) (string, func(), error) {
		return bootKnomit(ctx, cfg, lockPath)
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
			Handler: configInjectingHandlerWithDesktop(uiFS, desktopFS, boot.wait),
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
	// The two desktop-only windows. Both are lazy — no webview is built until
	// the user asks for one, and the log tailer only starts with the Logs
	// window. ctx (not a window's lifetime) is what stops that tailer, since it
	// deliberately keeps running while the window is hidden. See windows.go.
	aux := newAuxWindows(ctx, wapp, logFile)
	menu.Add("Logs…").OnClick(func(_ *application.Context) { aux.ShowLogs() })
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
			wapp.Dialog.Error().
				SetTitle("Knomit could not start").
				SetMessage(err.Error()).
				Show()
			wapp.Quit()
			return
		}
		// The bound port is only knowable here — it can differ from the
		// configured one when 19278 was taken. Published BEFORE the boot badge
		// clears, so nothing that reacts to "booted" can observe a zero port.
		nativeSvc.setEffectivePort(portFromBaseURL(apiBase))
		trayIcon.setBooting(false)
	}()

	// Everything that touches the OS notification centre starts HERE, not
	// above. ApplicationStarted is applicationDidFinishLaunching on macOS, so
	// it is the first point at which NSApplication exists and Wails has run
	// the notifications service's own startup guards — reaching
	// UNUserNotificationCenter before that raises rather than returns. See
	// configureUpdater.
	if startUpdates != nil {
		wapp.Event.OnApplicationEvent(events.Common.ApplicationStarted,
			func(*application.ApplicationEvent) { startUpdates() })
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

// bootKnomit starts everything behind the UI: the bundled CLI links, the
// in-process knomit application, and the HTTP server it is served on. It
// returns the API base URL and a teardown for what it started.
//
// This is the slow half of startup and it runs on a goroutine (see
// startServerBoot), so it must not touch Wails — nothing here does.
func bootKnomit(ctx context.Context, cfg config.Config, lockPath string) (string, func(), error) {
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

	// In-process server: API/MCP/git only (no UI), CORS for the Wails origin.
	a, err := knomitapp.New(ctx, cfg, knomitapp.Options{
		APIOnly:     true,
		CORSOrigins: wailsOrigins,
	})
	if err != nil {
		return "", nil, err
	}

	srv, port, err := bootServer(ctx, a.Handler(), lockPath, version.String(), cfg.Port)
	if err != nil {
		a.Close()
		return "", nil, err
	}
	apiBase := fmt.Sprintf("http://127.0.0.1:%d", port)
	log.Info().Str("api", apiBase).Int("port", port).Msg("knomit-desktop server up (API-only)")
	return apiBase, func() { srv.shutdown(); a.Close() }, nil
}

// apiBaseWaitTimeout caps how long a /config.js request will wait for the
// server to finish booting. Generous, because the alternative is a UI wired to
// no API at all: a boot that has not settled in this long is not slow, it is
// broken, and the 503 says so.
const apiBaseWaitTimeout = 90 * time.Second

// desktopPrefix is where the desktop-only UI bundle (Settings, Logs) lives.
// Everything outside it belongs to the shared knowledge app, which owns the
// root and its SPA fallback.
const desktopPrefix = "/desktop/"

// configInjectingHandler serves the shared knowledge UI at / with a live API
// base and no desktop-only tree. See configInjectingHandlerWithDesktop.
func configInjectingHandler(uiFS fs.FS, apiBase func(context.Context) (string, error)) http.Handler {
	return configInjectingHandlerWithDesktop(uiFS, nil, apiBase)
}

// configInjectingHandlerWithDesktop serves /config.js with the live API base,
// serves the embedded UI assets, falls back to index.html for client-side
// routes, and serves the desktop-only bundle under /desktop/.
//
// apiBase blocks until the server is up (see serverBoot.wait) rather than
// taking a fixed string, because the window can be opened before the server
// has finished booting. Holding this one request is what makes that harmless:
// the webview takes a moment longer to paint instead of loading against an
// address that does not exist yet.
//
// desktopFS may be nil, which disables the /desktop/ tree entirely.
func configInjectingHandlerWithDesktop(uiFS, desktopFS fs.FS, apiBase func(context.Context) (string, error)) http.Handler {
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
		if r.URL.Path == "/config.js" {
			ctx, cancel := context.WithTimeout(r.Context(), apiBaseWaitTimeout)
			defer cancel()
			base, err := apiBase(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("config.js: server not available")
				w.Header().Set("Retry-After", "2")
				http.Error(w, "knomit server is not available", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/javascript")
			// Never cache: the API base embeds the chosen port, which can differ
			// between launches (ephemeral fallback when 19278 is taken). A cached
			// copy would point the UI at a dead port.
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprintf(w, "window.__KNOMIT_API_BASE__ = %q;\nwindow.__KNOMIT_DESKTOP__ = true;\n", base)
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
