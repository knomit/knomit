//go:build desktop

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	knomitapp "knomit/internal/app"
	"knomit/internal/config"
	webui "knomit/web"

	"knomit/tools/desktop/internal/autostart"
	"knomit/tools/desktop/internal/paths"
	"knomit/tools/desktop/internal/singleinstance"
)

// trayIcon is the knomit logo shown in the system tray (rendered from
// web/public/logo.svg; see `make desktop-icons`).
//
//go:embed icon.png
var trayIcon []byte

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

	// In-process server: API/MCP/git only (no UI), CORS for the Wails origin.
	a, err := knomitapp.New(ctx, cfg, knomitapp.Options{
		APIOnly:     true,
		CORSOrigins: wailsOrigins,
	})
	if err != nil {
		return err
	}

	srv, port, err := bootServer(ctx, a.Handler(), lockPath, version)
	if err != nil {
		a.Close()
		return err
	}
	// Wails calls os.Exit on quit, so Go defers in run() do not fire. Run
	// cleanup via Wails' OnShutdown hook instead, guarded by a sync.Once so it
	// happens exactly once regardless of exit path.
	var once sync.Once
	shutdown := func() { once.Do(func() { srv.shutdown(); a.Close() }) }
	apiBase := fmt.Sprintf("http://127.0.0.1:%d", port)
	log.Info().Str("api", apiBase).Int("port", port).Msg("knomit-desktop server up (API-only)")

	uiFS, err := webui.FS()
	if err != nil {
		shutdown() // boot succeeded; tear it down before returning
		return fmt.Errorf("embedded UI: %w", err)
	}

	wapp := application.New(application.Options{
		Name: "Knomit",
		Assets: application.AssetOptions{
			Handler: configInjectingHandler(uiFS, apiBase),
		},
		Services: []application.Service{
			application.NewService(&NativeService{}),
		},
		OnShutdown: shutdown,
	})

	window := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Knomit",
		Width:  1200,
		Height: 800,
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
	tray.SetIcon(trayIcon)
	menu := wapp.NewMenu()
	menu.Add("Open Knomit").OnClick(func(_ *application.Context) {
		window.Show()
		window.Focus()
	})
	settings := menu.AddSubmenu("Settings")
	addAutostartItem(settings)
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(_ *application.Context) { wapp.Quit() })
	tray.SetMenu(menu)

	// Quit the Wails run loop when the context is cancelled (SIGINT/SIGTERM),
	// so deferred shutdown (lockfile removal, app close) runs on a clean exit.
	go func() {
		<-ctx.Done()
		wapp.Quit()
	}()

	err = wapp.Run()
	shutdown() // in case Run returns without triggering OnShutdown
	return err
}

// configInjectingHandler serves /config.js with the live API base, serves the
// embedded UI assets, and falls back to index.html for client-side routes.
func configInjectingHandler(uiFS fs.FS, apiBase string) http.Handler {
	fileServer := http.FileServer(http.FS(uiFS))
	configJS := fmt.Sprintf("window.__KNOMIT_API_BASE__ = %q;\nwindow.__KNOMIT_DESKTOP__ = true;\n", apiBase)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config.js" {
			w.Header().Set("Content-Type", "application/javascript")
			// Never cache: the API base embeds the chosen port, which can differ
			// between launches (ephemeral fallback when 19278 is taken). A cached
			// copy would point the UI at a dead port.
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(configJS))
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

// addAutostartItem adds a "Start at login" checkbox bound to the platform
// autostart toggler.
func addAutostartItem(menu *application.Menu) {
	tog := autostart.New()
	enabled, _ := tog.Enabled()
	item := menu.Add("Start at login")
	item.SetChecked(enabled)
	item.OnClick(func(_ *application.Context) {
		if on, _ := tog.Enabled(); on {
			_ = tog.Disable()
			item.SetChecked(false)
		} else {
			_ = tog.Enable()
			item.SetChecked(true)
		}
	})
}
