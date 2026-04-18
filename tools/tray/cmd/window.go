package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func windowCmd() *cobra.Command {
	var url string
	c := &cobra.Command{
		Use:    "window",
		Short:  "Open a webview window pointing at URL (internal use)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if url == "" {
				return fmt.Errorf("--url is required")
			}
			return runWindow(url)
		},
	}
	c.Flags().StringVar(&url, "url", "", "URL to load in the webview")
	return c
}

func runWindow(url string) error {
	// Filled in by Task 8.
	return fmt.Errorf("window not implemented yet")
}
