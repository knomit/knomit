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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/homelock"
)

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

// TestRestoreRefusesWhileAServerHoldsTheHome is the real enforcement behind
// "run it against a STOPPED server". Before the home claim existed the only
// guard was the agent's "am I replicating this path" check, which can never fire
// here: restore spawns a FRESH agent with an empty tracked set. So the most
// destructive command in the product would happily rename over a database a
// running knomit had open.
func TestRestoreRefusesWhileAServerHoldsTheHome(t *testing.T) {
	home := backupEnabledHome(t)
	held, err := homelock.Acquire(home)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	_, err = runCmd(t, restoreCmd(), "--control")
	if err == nil {
		t.Fatal("restore proceeded while a server held KNOMIT_HOME")
	}
	if !errors.Is(err, homelock.ErrHeld) {
		t.Fatalf("error = %v, want it to wrap homelock.ErrHeld", err)
	}
	// The message has to tell the operator what to do, and that a CRASHED
	// server will not block them — recovery is when they need this command.
	for _, want := range []string{"Stop the server", "crash"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRestoreLeavesNoStrayHomeDirectory: taking a claim on KNOMIT_HOME is not a
// reason to create it. A typo'd path should fail, not quietly become a new empty
// home — which is worse than a plain error, because the restore would then
// happily fill it and the operator's real data would still be elsewhere.
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

// TestRestoreReleasesTheHomeWhenItFails: the claim is held across the restore,
// so a command that exits without releasing would lock the operator out of
// their own recovery until they noticed why.
func TestRestoreReleasesTheHomeWhenItFails(t *testing.T) {
	home := backupEnabledHome(t)
	// Fails at backup.Open (no agent binary anywhere), well after the claim.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("KNOMIT_BACKUP_AGENT", filepath.Join(t.TempDir(), "absent"))
	if _, err := runCmd(t, restoreCmd(), "--control"); err == nil {
		t.Fatal("restore succeeded with no agent binary; expected it to fail after taking the claim")
	}
	l, err := homelock.Acquire(home)
	if err != nil {
		t.Fatalf("KNOMIT_HOME still claimed after restore failed: %v", err)
	}
	_ = l.Release()
}
