package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

func TestRestoreIsRegistered(t *testing.T) {
	for _, c := range RootCmd().Commands() {
		if c.Name() == "restore" {
			return
		}
	}
	t.Error("knomit restore is not registered on the root command")
}
