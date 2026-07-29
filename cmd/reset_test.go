package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestResetRefusesWhenBackupIsEnabled: reset deletes a repo's database while
// the registry still lists the repo. On a replicated instance that is not a
// local wipe — it is a change the next boot has to reconcile against a replica
// that still holds the data, and doing that by accident is not recoverable by
// re-running anything.
func TestResetRefusesWhenBackupIsEnabled(t *testing.T) {
	home := backupEnabledHome(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runCmd(t, resetCmd(), "--name", "core")
	if err == nil {
		t.Fatal("reset succeeded on a backup-enabled instance without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error %q does not tell the operator how to proceed deliberately", err)
	}
	if _, serr := os.Stat(dbPath); serr != nil {
		t.Errorf("the refused reset deleted %s anyway: %v", dbPath, serr)
	}
}

// TestResetForceProceedsWhenBackupIsEnabled: the guard is a speed bump, not a
// wall. An operator who means it must have a way through.
func TestResetForceProceedsWhenBackupIsEnabled(t *testing.T) {
	home := backupEnabledHome(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runCmd(t, resetCmd(), "--name", "core", "--force"); err != nil {
		t.Fatalf("reset --force: %v", err)
	}
	if _, serr := os.Stat(dbPath); !os.IsNotExist(serr) {
		t.Errorf("reset --force left %s in place (%v)", dbPath, serr)
	}
}

// TestResetNeedsNoForceWhenBackupIsOff: the guard exists for replicated
// instances only, and making everyone type --force would train them to.
func TestResetNeedsNoForceWhenBackupIsOff(t *testing.T) {
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
// reported before anything else, including the backup guard.
func TestResetStillRequiresAName(t *testing.T) {
	backupEnabledHome(t)
	_, err := runCmd(t, resetCmd())
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("reset with no --name = %v, want the missing-flag error", err)
	}
}
