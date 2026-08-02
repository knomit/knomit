//go:build !desktop

// These tests assert SERVER-build behaviour: they need backup.enabled to be
// settable, and in the desktop build it is not — internal/config forces it off
// under the `desktop` tag, structurally, so no environment variable or config
// file can turn it on (see internal/config/backup_desktop.go and the
// project-owner ruling recorded there).
//
// The cmd package is the `knomit` server binary's command set and is never
// compiled into knomit-desktop, so building it with that tag is not a real
// configuration — but `go test -tags desktop ./...` does compile it, and a test
// that asserts a behaviour the tag removes would fail there for no useful
// reason. The tag on this file says which build these belong to rather than
// leaving them to fail and be explained away.

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestoreLeavesNoStrayHomeDirectory: restore is the one command that writes
// databases into KNOMIT_HOME, and it deliberately does not CREATE that home. A
// typo'd path must fail rather than quietly become a new empty home — which is
// worse than a plain error, because the restore would then happily fill it and
// the operator's real data would still be elsewhere.
func TestRestoreLeavesNoStrayHomeDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "typo", "knomit")
	t.Setenv("KNOMIT_HOME", missing)
	t.Setenv("KNOMIT_AGENT_NAME", "test-agent")
	t.Setenv("KNOMIT_BACKUP_ENABLED", "true")
	t.Setenv("KNOMIT_BACKUP_URL", "file://"+t.TempDir())

	_, err := runCmd(t, restoreCmd(), "--control")
	if err == nil {
		t.Fatal("restore succeeded against a nonexistent KNOMIT_HOME")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the missing path", err)
	}
	if _, serr := os.Stat(missing); !os.IsNotExist(serr) {
		t.Errorf("restore created %s before failing (%v)", missing, serr)
	}
}
