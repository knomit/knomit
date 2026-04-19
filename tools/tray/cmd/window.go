package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	webview "github.com/webview/webview_go"
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
			if url == "" {
				return fmt.Errorf("--url is required")
			}
			return runWindow(url, title, w, h)
		},
	}
	c.Flags().StringVar(&url, "url", "", "URL to load")
	c.Flags().StringVar(&title, "title", "Knomit", "Window title")
	c.Flags().IntVar(&w, "width", 1200, "Window width")
	c.Flags().IntVar(&h, "height", 800, "Window height")
	return c
}

func runWindow(url, title string, w, h int) error {
	// WKWebView requires the GUI event loop on the main OS thread.
	runtime.LockOSThread()
	wv := webview.New(false)
	if wv == nil {
		return fmt.Errorf("webview: failed to create native window")
	}
	defer wv.Destroy()

	wv.SetTitle(title)
	wv.SetSize(w, h, webview.HintNone)
	wv.Navigate(url)
	wv.Run() // blocks until the window is closed
	return nil
}
