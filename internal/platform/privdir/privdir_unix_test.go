//go:build !windows

package privdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestEnsure_CreatesWith0700(t *testing.T) {
	logs := captureLog(t)
	home := filepath.Join(t.TempDir(), "a", "home")

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, home); m != 0o700 {
		t.Errorf("created with mode %v, want 0700", m)
	}
	if logs.Len() != 0 {
		t.Errorf("a fresh private directory logged: %s", logs)
	}
}

// The case every existing install is in: the root was created 0755 by an
// earlier writer, and this user owns it. It is tightened, and that is said
// once, at info level.
func TestEnsure_ExistingWideOwnedDirIsTightened(t *testing.T) {
	logs := captureLog(t)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Chmod, not Mkdir's mode: the umask would otherwise decide it.
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, home); m != 0o700 {
		t.Errorf("mode after Ensure = %v, want 0700", m)
	}
	out := logs.String()
	if !strings.Contains(out, `"level":"info"`) || !strings.Contains(out, "tightened data root to 0700 (was 755)") || !strings.Contains(out, home) {
		t.Errorf("no info line naming %s and the old mode: %q", home, out)
	}

	// The next boot finds it private and says nothing.
	logs.Reset()
	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("second Ensure logged: %s", logs)
	}
}

func TestEnsure_ExistingPrivateDirIsQuietAndUnchanged(t *testing.T) {
	logs := captureLog(t)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Stricter than 0700 is private too, and must not be widened to it.
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, home); m != 0o500 {
		t.Errorf("mode after Ensure = %v, want 0500 left alone", m)
	}
	if logs.Len() != 0 {
		t.Errorf("a private directory logged: %s", logs)
	}
}

// A root that is a symlink to a wide directory is warned about and never
// chmodded through the link, even though this user owns the target.
func TestEnsure_SymlinkedWideRootIsWarnedNotChanged(t *testing.T) {
	logs := captureLog(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Symlink(target, home); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, target); m != 0o755 {
		t.Errorf("Ensure chmodded through the symlink: target mode %v, want 0755", m)
	}
	out := logs.String()
	if !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, "symlink") {
		t.Errorf("no warning about the symlinked root: %q", out)
	}
}

// A wide root owned by someone else (or reached as root via sudo, which is
// the euid check) is warned about and not chmodded. It cannot be built
// without a second account, so it is covered by reading ensure's owner
// check, not by a test.

// assertNotPrivate: err is ErrNotPrivate and its message carries each want.
func assertNotPrivate(t *testing.T, err error, want ...string) {
	t.Helper()
	if !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("Verify: %v, want ErrNotPrivate", err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("Verify error %q does not say %q", err, w)
		}
	}
}

// Ensure only warns about a symlinked root; Verify refuses it, even when the
// target is a private directory this user owns: the link can be repointed.
func TestVerify_RefusesASymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Verify(target); err != nil {
		t.Fatalf("fixture: the target itself is not private: %v", err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Symlink(target, home); err != nil {
		t.Fatal(err)
	}
	assertNotPrivate(t, Verify(home), home, "symlink")
}

// Group or other bits, each on its own, are refused, and the error names
// the chmod that fixes it. Verify does not tighten (that is Ensure's job).
func TestVerify_RefusesGroupOrOtherBits(t *testing.T) {
	for _, mode := range []os.FileMode{0o750, 0o710, 0o705, 0o701, 0o755} {
		home := filepath.Join(t.TempDir(), "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(home, mode); err != nil {
			t.Fatal(err)
		}
		assertNotPrivate(t, Verify(home), home, "chmod 700 "+home)
		if m := modeOf(t, home); m != mode {
			t.Errorf("Verify changed the mode: %v, want %v", m, mode)
		}
	}
}

// A 0700 directory owned by another uid is not private to this user: its
// owner can read it. Built by making Verify see another euid, since a test
// cannot chown to a second account.
func TestVerify_RefusesADirOwnedBySomeoneElse(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if err := Verify(home); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	real := geteuid()
	geteuid = func() int { return real + 1 }
	t.Cleanup(func() { geteuid = os.Geteuid })
	assertNotPrivate(t, Verify(home), home, "owned by uid")
}

func TestVerify_RefusesARegularFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertNotPrivate(t, Verify(file), file, "not a directory")
}
