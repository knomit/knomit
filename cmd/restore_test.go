package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"knomit/internal/homelock"
)

// runCmd executes a subcommand with args, capturing its output. Usage text and
// cobra's own error echo are silenced so the assertions read the command's own
// error and nothing else.
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

// TestRestoreRequiresATarget: restore replaces a database, so it must never
// guess which one. There is no default repo, and defaulting to control.db would
// make the most destructive invocation the shortest one to type.
func TestRestoreRequiresATarget(t *testing.T) {
	// A config that would FAIL to load, so the assertion below also proves the
	// flag check runs first. Booting the backup client spawns the agent and
	// round-trips the object store; reporting a missing flag after that — or,
	// worse, masking it behind an unrelated config error — is the failure
	// cmd/reset and cmd/verify already avoid.
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_BACKUP_ENABLED", "true")
	t.Setenv("KNOMIT_BACKUP_URL", "")

	_, err := runCmd(t, restoreCmd())
	if err == nil {
		t.Fatal("restore with no target succeeded; want a refusal")
	}
	if !strings.Contains(err.Error(), "--repo") || !strings.Contains(err.Error(), "--control") {
		t.Errorf("error %q does not name the two ways to pick a target", err)
	}
	if strings.Contains(err.Error(), "backup.url") {
		t.Errorf("error %q is the config failure; the flag check must run before config.Load", err)
	}
}

// TestRestoreRejectsBothTargets: --repo and --control name different databases,
// and silently preferring one would overwrite something the operator did not
// ask for.
func TestRestoreRejectsBothTargets(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	_, err := runCmd(t, restoreCmd(), "--control", "--repo", "core")
	if err == nil {
		t.Fatal("restore with both --control and --repo succeeded; want a refusal")
	}
	if !strings.Contains(err.Error(), "--repo") || !strings.Contains(err.Error(), "--control") {
		t.Errorf("error %q does not explain the conflict", err)
	}
}

func TestRestoreRejectsAMalformedTimestamp(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	_, err := runCmd(t, restoreCmd(), "--control", "--timestamp", "last tuesday")
	if err == nil {
		t.Fatal("restore accepted a non-RFC3339 timestamp")
	}
	if !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("error %q does not say what format is wanted", err)
	}
}

// TestRestoreRefusesWhenBackupIsDisabled: there is no replica to restore from,
// and the useful answer says which setting is missing.
func TestRestoreRefusesWhenBackupIsDisabled(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_BACKUP_ENABLED", "false")

	_, err := runCmd(t, restoreCmd(), "--control")
	if err == nil {
		t.Fatal("restore succeeded with backup disabled")
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Errorf("error %q does not mention backup configuration", err)
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

func TestRestoreIsRegistered(t *testing.T) {
	for _, c := range RootCmd().Commands() {
		if c.Name() == "restore" {
			return
		}
	}
	t.Error("knomit restore is not registered on the root command")
}
