package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags "-X knomit/tools/tray/cmd.version=X.Y.Z".
var version = "0.1.0-dev"

// RootCmd returns the top-level knomit-tray command. Default action: start the tray.
// The implementation of runTray is provided by tray_darwin.go / tray_linux.go
// depending on GOOS.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "knomit-tray",
		Short: "Knomit system-tray app",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTray(cmd.Context())
		},
	}
	root.AddCommand(windowCmd())
	return root
}

// locateKnomit finds the `knomit` binary in this order:
//  1. $KNOMIT_BIN
//  2. same directory as knomit-tray
//  3. $PATH
func locateKnomit() (string, error) {
	if env := os.Getenv("KNOMIT_BIN"); env != "" {
		return env, nil
	}
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "knomit")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("knomit"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("knomit binary not found (set KNOMIT_BIN, place next to knomit-tray, or add to PATH)")
}
