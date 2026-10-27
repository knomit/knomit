// Package gitfixture creates the bare git repositories tests use as remotes.
// Every test that makes a bare repository goes through it (#651, #654
// review S3), so each one has git's auto-maintenance turned off.
//
// WHY. Every push into a file:// test remote runs the real git-receive-pack:
// the git CLI does, and so does go-git's file transport, which execs the git
// binary. After the push, receive-pack starts
// `git maintenance run --auto --detach`. That child takes
// objects/maintenance.lock BEFORE it detaches and removes it later, after the
// push has returned. So a test that snapshots the remote's bytes sees a file
// come or go, and t.TempDir's RemoveAll races the child ("directory not
// empty").
//
// receive-pack reads maintenance.auto=false in the bare repository itself,
// whatever environment it was started with, so no child starts at all. A
// process-wide variable would not reach every git: test/testenv's git runs
// with a stripped environment.
//
// It imports nothing from knomit, so every package's tests can use it,
// package store's own included.
//
// Test-only: it imports "testing", and TestHarnessNotLinkedIntoShippedBinaries
// keeps it out of every shipped binary.
package gitfixture

import (
	"os"
	"os/exec"
	"testing"
)

// InitBare creates a bare git repository at dir with initial branch branch
// ("" means git's default) and auto-maintenance off, and returns dir.
func InitBare(t testing.TB, dir, branch string) string {
	t.Helper()
	args := []string{"init", "-q", "--bare"}
	if branch != "" {
		args = append(args, "--initial-branch="+branch)
	}
	run(t, append(args, dir)...)
	NoAutoMaintenance(t, dir)
	return dir
}

// CloneBare creates a bare clone of src at dst with auto-maintenance off, and
// returns dst.
func CloneBare(t testing.TB, src, dst string) string {
	t.Helper()
	run(t, "clone", "-q", "--bare", src, dst)
	NoAutoMaintenance(t, dst)
	return dst
}

// NoAutoMaintenance turns git's auto-maintenance off in the repository at
// dir. Use it for a bare repository made some other way, such as go-git's
// PlainInit or PlainClone with isBare set.
func NoAutoMaintenance(t testing.TB, dir string) {
	t.Helper()
	run(t, "-C", dir, "config", "maintenance.auto", "false")
}

func run(t testing.TB, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
