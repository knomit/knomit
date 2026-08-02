package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runCmd executes a cobra command with its output captured. It lived in
// restore_test.go until `knomit restore` was removed; reset is the only caller
// left.
func runCmd(t *testing.T, c *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetArgs(args)
	return out.String(), c.Execute()
}

// backupEnabledHome configures a KNOMIT_HOME with replication switched on and
// returns it. No agent binary is needed: every guard under test must fire
// before anything talks to the replica.
func backupEnabledHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_AGENT_NAME", "test-agent")
	t.Setenv("KNOMIT_BACKUP_ENABLED", "true")
	t.Setenv("KNOMIT_BACKUP_URL", "file://"+t.TempDir())
	return home
}

// TestResetDeletesTheDatabase: reset's whole job is removing the named repo's
// database file, so a "success" that leaves it in place is the failure worth
// pinning.
func TestResetDeletesTheDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_BACKUP_ENABLED", "false")
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runCmd(t, resetCmd(), "--name", "core"); err != nil {
		t.Fatalf("reset with backup off: %v", err)
	}
	if _, serr := os.Stat(dbPath); !os.IsNotExist(serr) {
		t.Errorf("reset left %s in place (%v)", dbPath, serr)
	}
}

// TestResetStillRequiresAName pins the existing ordering: the missing flag is
// reported before anything else, including config load.
func TestResetStillRequiresAName(t *testing.T) {
	backupEnabledHome(t)
	_, err := runCmd(t, resetCmd())
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("reset with no --name = %v, want the missing-flag error", err)
	}
}
