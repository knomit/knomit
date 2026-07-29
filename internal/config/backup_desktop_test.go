//go:build desktop

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDesktopBuildCannotEnableBackup is the guard behind a project-owner
// ruling: backup is available for SERVER builds only, and the desktop app must
// never trigger it.
//
// It matters because the desktop app runs the knomit server in-process. A user
// with `backup.enabled = true` in ~/.knomit/knomit.toml — set for their CLI
// server, which is the ordinary reason to have it — who then opens the desktop
// app would otherwise get a SECOND replicator against the same object-store
// prefix. Two litestream agents writing one LTX chain is the condition knomit
// deliberately never auto-repairs.
//
// Both routes are asserted together, because either alone would be a way back
// in: the environment variable AND a config file on disk.
//
// This test only runs under `-tags desktop`, as every other desktop test in
// this repository does. `make test` does not pass the tag; `go build/vet/test
// -tags desktop` is the gate.
func TestDesktopBuildCannotEnableBackup(t *testing.T) {
	home := t.TempDir()
	toml := filepath.Join(home, "knomit.toml")
	if err := os.WriteFile(toml, []byte("[backup]\nenabled = true\nurl = \"s3://bucket/prefix\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_BACKUP_ENABLED", "true")
	t.Setenv("KNOMIT_BACKUP_URL", "s3://bucket/prefix")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backup.Enabled {
		t.Fatal("the desktop build resolved backup.enabled = true; it runs the server in-process, " +
			"so this is a second replicator against the same prefix as the user's CLI server")
	}
}

// TestDesktopBuildDoesNotRefuseAnIncompleteBackupConfig: a desktop user whose
// config says `enabled = true` with no url must get a working app, not a boot
// refused over a setting that is being ignored anyway. In a server build that
// combination is a hard error, and it should stay one there.
func TestDesktopBuildDoesNotRefuseAnIncompleteBackupConfig(t *testing.T) {
	home := t.TempDir()
	toml := filepath.Join(home, "knomit.toml")
	if err := os.WriteFile(toml, []byte("[backup]\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with backup enabled and no url = %v; the desktop build ignores backup, so this "+
			"must not fail the boot", err)
	}
	if cfg.Backup.Enabled {
		t.Error("backup.enabled survived the desktop override")
	}
}
