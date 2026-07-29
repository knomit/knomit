package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

// depsOf returns the transitive import graph of pkg via `go list -deps`, which
// resolves imports without compiling and so needs no native libraries.
func depsOf(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s failed: %v\n%s", pkg, err, out)
	}
	var deps []string
	for _, line := range strings.Split(string(out), "\n") {
		if dep := strings.TrimSpace(line); dep != "" {
			deps = append(deps, dep)
		}
	}
	return deps
}

// TestKnomitBinaryDoesNotLinkLitestream is the enforcement behind the whole
// reason replication runs in a child process.
//
// litestream v0.5 drives SQLite through modernc.org/sqlite; knomit drives the
// same files through the cgo mattn/go-sqlite3 build and cannot switch, because
// sqlite-vec has no modernc build. Two SQLite BUILDS in ONE process do not see
// each other's file locks — POSIX advisory record locks do not conflict between
// descriptors held by the same process, and SQLite's compensating per-process
// inode table belongs to a single build. That was demonstrated, not theorised:
// knomit's close deleted litestream's -wal while litestream held a read lock,
// 3 runs out of 3.
//
// So this is not a tidiness rule. One import — a helper reached for because it
// was convenient, a constant borrowed from litestream — links the second SQLite
// build back into knomit and restores a corruption mode whose symptoms appear
// far from the cause. The knomit-backup agent is where litestream lives; that
// binary is deliberately not checked here.
func TestKnomitBinaryDoesNotLinkLitestream(t *testing.T) {
	for _, dep := range depsOf(t, "knomit") {
		if strings.HasPrefix(dep, "github.com/benbjohnson/litestream") || strings.HasPrefix(dep, "modernc.org/") {
			t.Errorf("the knomit binary transitively imports %q — replication runs in the knomit-backup "+
				"child process precisely so litestream's SQLite build is never linked beside knomit's. "+
				"Route whatever needs it through internal/backupproto instead.", dep)
		}
	}
}

// TestRuntimeobsDoesNotImportBackup keeps the diagnostics port free of the
// replication client.
//
// runtimeobs serves pprof, expvar and /metrics and is meant to be usable by
// anything; importing internal/backup would drag the agent protocol, the child
// process supervisor and their dependencies into every consumer of it. The
// backup status it reports arrives through an injected hook over a locally
// declared mirror type (runtimeobs.BackupDBStatus), and this is what stops the
// obvious shortcut — "just import the real type" — from being taken later.
func TestRuntimeobsDoesNotImportBackup(t *testing.T) {
	for _, dep := range depsOf(t, "knomit/internal/runtimeobs") {
		if dep == "knomit/internal/backup" {
			t.Error("knomit/internal/runtimeobs transitively imports knomit/internal/backup — " +
				"the diagnostics port must stay dependency-free in the direction of the app. " +
				"Mirror the type locally and take a hook, as Options.BackupStatus does.")
		}
	}
}
