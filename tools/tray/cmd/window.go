package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	webview "github.com/webview/webview_go"

	"knomit/tools/tray/internal/lockfile"
	"knomit/tools/tray/internal/paths"
)

func windowCmd() *cobra.Command {
	var (
		url   string
		title string
		w, h  int
	)
	c := &cobra.Command{
		Use:    "window",
		Short:  "Open a webview window (internal use — spawned by the tray)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			lp, err := paths.LockfilePath()
			if err != nil {
				return err
			}
			resolved, err := resolveURL(url, lp)
			if err != nil {
				return err
			}
			return runWindow(resolved, title, w, h)
		},
	}
	c.Flags().StringVar(&url, "url", "", "URL to load (defaults to the running tray's lockfile)")
	c.Flags().StringVar(&title, "title", "Knomit", "Window title")
	c.Flags().IntVar(&w, "width", 1200, "Window width")
	c.Flags().IntVar(&h, "height", 800, "Window height")
	return c
}

// resolveURL returns the explicit url if non-empty; otherwise it reads the
// lockfile at lockPath and constructs http://127.0.0.1:<port>. Extracted as
// a pure helper so it can be unit-tested without depending on webview's CGO.
func resolveURL(url, lockPath string) (string, error) {
	if url != "" {
		return url, nil
	}
	info, err := lockfile.Read(lockPath)
	if err != nil {
		return "", fmt.Errorf("no --url given and no lockfile at %s (is knomit-tray running?): %w", lockPath, err)
	}
	if info.Port <= 0 {
		return "", fmt.Errorf("lockfile at %s has no valid port", lockPath)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", info.Port), nil
}

func runWindow(url, title string, w, h int) error {
	// WKWebView (macOS) and WebKitGTK (Linux) both require the GUI loop
	// on the main OS thread.
	runtime.LockOSThread()
	wv := webview.New(false)
	if wv == nil {
		return fmt.Errorf("webview: failed to create native window")
	}
	defer wv.Destroy()

	wv.SetTitle(title)
	wv.SetSize(w, h, webview.HintNone)
	wv.Navigate(url)
	wv.Run()
	return nil
}
