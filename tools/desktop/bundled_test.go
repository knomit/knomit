//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLinkInto(t *testing.T) {
	if !canSymlink(t) {
		t.Skip("this host cannot create symlinks (Windows without Developer Mode or an elevated shell)")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "kb")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	// First install: creates the link pointing at target.
	link, err := linkInto(binDir, target)
	if err != nil {
		t.Fatalf("first linkInto: %v", err)
	}
	if want := filepath.Join(binDir, "kb"); link != want {
		t.Errorf("link path = %q, want %q", link, want)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Errorf("link target = %q, want %q", got, target)
	}

	// Idempotent: a second call with the same target leaves the link intact.
	if _, err := linkInto(binDir, target); err != nil {
		t.Fatalf("second linkInto: %v", err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Errorf("after re-run, link target = %q, want %q", got, target)
	}

	// Refresh: a stale link (app moved/updated) is repointed at the new target.
	newTarget := filepath.Join(dir, "src2", "kb")
	if err := os.MkdirAll(filepath.Dir(newTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newTarget, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := linkInto(binDir, newTarget); err != nil {
		t.Fatalf("refresh linkInto: %v", err)
	}
	if got, _ := os.Readlink(link); got != newTarget {
		t.Errorf("after refresh, link target = %q, want %q", got, newTarget)
	}
}

func TestLinkInto_ReplacesRegularFile(t *testing.T) {
	if !canSymlink(t) {
		t.Skip("this host cannot create symlinks (Windows without Developer Mode or an elevated shell)")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "kb")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-existing regular file at the link path must be replaced by the link.
	if err := os.WriteFile(filepath.Join(binDir, "kb"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link, err := linkInto(binDir, target)
	if err != nil {
		t.Fatalf("linkInto: %v", err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Errorf("link target = %q, want %q (regular file not replaced)", got, target)
	}
}

// The installers must fail gracefully (not panic) when the tool does not sit
// next to the running executable — the dev `go run` case. The test binary has
// no siblings, so this exercises the skip path for both.
func TestInstallTool_NoBundledTool(t *testing.T) {
	if _, err := installBridgeTool(t.TempDir()); err == nil {
		t.Error("expected an error when no bundled kb is present")
	}
	if _, err := installOKFTool(t.TempDir()); err == nil {
		t.Error("expected an error when no bundled knomit-okf is present")
	}
}

// TestInstallSymlink_EachInstallerResolvesItsOwnBinary pins that the two
// installers ask for DISTINCT binaries. They now share one code path, which is
// exactly what makes it possible to pass the same exec name twice and hand a
// user a "knomit-okf" that is really the MCP bridge.
//
// Both resolve relative to os.Executable(), which a test cannot redirect, so
// both necessarily fail here — but the error names the binary each one looked
// for, and that is the wiring under test.
func TestInstallTool_EachInstallerResolvesItsOwnBinary(t *testing.T) {
	_, bridgeErr := installBridgeTool(t.TempDir())
	_, okfErr := installOKFTool(t.TempDir())
	if bridgeErr == nil || okfErr == nil {
		t.Fatal("expected both installers to fail with no bundled tools present")
	}
	if !strings.Contains(bridgeErr.Error(), bridgeExecName) {
		t.Errorf("bridge installer looked for the wrong binary: %v", bridgeErr)
	}
	if !strings.Contains(okfErr.Error(), okfExecName) {
		t.Errorf("okf installer looked for the wrong binary: %v", okfErr)
	}
	if strings.Contains(okfErr.Error(), bridgeExecName) {
		t.Errorf("okf installer resolved the BRIDGE binary — the tools are crossed: %v", okfErr)
	}
}

func TestCopyIntoReplacesStaleContent(t *testing.T) {
	// On Linux the bundled tools are COPIED, not symlinked: inside an AppImage
	// the source lives in a FUSE mount that vanishes when the app exits, so a
	// symlink would dangle and every MCP client wired to <home>/bin would break.
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "kb")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatalf("copyInto: %v", err)
	}
	if want := filepath.Join(binDir, "kb"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}

	// It must be a real file, not a link into the (future) mount.
	fi, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("copyInto produced a symlink; the AppImage mount it points into will vanish")
	}
	// Windows has no execute bit — a file is executable by extension, and Go
	// reports 0666 for anything writable. The bit is load-bearing on macOS and
	// Linux, where an MCP client could not launch the tool without it.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("copied tool is not executable: mode %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(got); string(b) != "v1" {
		t.Errorf("content = %q, want v1", b)
	}

	// An update replaces the source; the installed copy must follow.
	if err := os.WriteFile(target, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := copyInto(binDir, target); err != nil {
		t.Fatalf("second copyInto: %v", err)
	}
	if b, _ := os.ReadFile(got); string(b) != "v2" {
		t.Errorf("content after refresh = %q, want v2", b)
	}

	// No staging temp files left behind.
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("bin dir holds %v, want just the installed tool", names)
	}
}

func TestCopyIntoReplacesAnExistingSymlink(t *testing.T) {
	// Upgrading from a tarball install leaves a symlink behind at the
	// destination. It must be replaced by a real file, not written through.
	if !canSymlink(t) {
		t.Skip("this host cannot create the symlink this fixture needs")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "kb")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "gone", "kb")
	if err := os.Symlink(stale, filepath.Join(binDir, "kb")); err != nil {
		t.Fatal(err)
	}

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatalf("copyInto over a stale symlink: %v", err)
	}
	fi, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("stale symlink survived; want a real file")
	}
	if b, _ := os.ReadFile(got); string(b) != "new" {
		t.Errorf("content = %q, want new", b)
	}
	// The dangling link's target must not have been created by writing through it.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("wrote through the stale symlink instead of replacing it")
	}
}

// The copied tool must keep working after the source disappears — that is the
// entire point on Linux, where the source lives in a FUSE mount torn down when
// the app exits.
func TestCopyIntoSurvivesSourceRemoval(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mnt", "usr", "bin")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mount, "kb")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the AppImage unmounting on app exit.
	if err := os.RemoveAll(filepath.Join(dir, "mnt")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("installed tool unreadable after the source vanished: %v", err)
	}
	if string(b) != "bin" {
		t.Errorf("content = %q, want bin", b)
	}
}

// The per-platform dispatch itself, exercised for both branches regardless of
// the host running the tests. On a macOS CI runner the linux branch would
// otherwise never execute, and it is the branch the AppImage depends on.
func TestPlaceToolDispatchesPerPlatform(t *testing.T) {
	tests := []struct {
		goos        string
		wantSymlink bool
	}{
		{"linux", false},   // AppImage: the source mount vanishes, so copy
		{"darwin", true},   // .app at a stable path, updated in place
		{"windows", false}, // symlinks need a privilege ordinary users lack
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			// The darwin row calls os.Symlink on the HOST, whatever goos says.
			// Windows refuses that without SeCreateSymbolicLinkPrivilege, so
			// the row fails on a normal account and passes in an elevated
			// shell — exactly the difference that must not decide whether the
			// suite is green.
			if tt.wantSymlink && !canSymlink(t) {
				t.Skip("this host cannot create symlinks (Windows without Developer Mode or an elevated shell)")
			}

			dir := t.TempDir()
			target := filepath.Join(dir, "src", "kb")
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
				t.Fatal(err)
			}

			got, err := placeTool(tt.goos, filepath.Join(dir, "bin"), target)
			if err != nil {
				t.Fatalf("placeTool(%s): %v", tt.goos, err)
			}
			fi, err := os.Lstat(got)
			if err != nil {
				t.Fatal(err)
			}
			isSymlink := fi.Mode()&os.ModeSymlink != 0
			if isSymlink != tt.wantSymlink {
				t.Errorf("placeTool(%s) produced symlink=%v, want %v", tt.goos, isSymlink, tt.wantSymlink)
			}
		})
	}
}

// copyInto runs on EVERY app start. Without a skip the app would decompress
// and rewrite both bundled tools out of the AppImage's squashfs image on every
// launch, for bytes it already has.
func TestCopyIntoSkipsAnUnchangedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "kb")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("bundle v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatalf("first copyInto: %v", err)
	}
	first, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}

	// Second call with an untouched source must not re-copy. A re-copy
	// renames a fresh temp file over the destination, so the inode changes —
	// os.SameFile is what detects that, where comparing content could not.
	if _, err := copyInto(binDir, target); err != nil {
		t.Fatalf("second copyInto: %v", err)
	}
	second, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, second) {
		t.Error("copyInto re-copied an unchanged file; every app start would rewrite it")
	}
}

// The skip must not survive an update. A new AppImage carries a fresh mtime,
// which is exactly what upToDate keys on.
func TestCopyIntoRecopiesWhenTheSourceChanges(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "kb")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("bundle v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatalf("first copyInto: %v", err)
	}

	// Same LENGTH, different bytes and a newer mtime — the case a size-only
	// check would wrongly skip, shipping a stale bridge against a new app.
	if err := os.WriteFile(target, []byte("bundle v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := copyInto(binDir, target); err != nil {
		t.Fatalf("second copyInto: %v", err)
	}
	if b, _ := os.ReadFile(got); string(b) != "bundle v2" {
		t.Errorf("content = %q, want bundle v2 — an updated source was skipped", b)
	}
}

// A symlink at the destination must be replaced even when its size and mtime
// match, because os.Lstat on a symlink reports the LINK. Getting this wrong
// would leave the dangling-into-the-FUSE-mount link that this whole code path
// exists to eliminate.
func TestCopyIntoReplacesASymlinkThatLooksCurrent(t *testing.T) {
	if !canSymlink(t) {
		t.Skip("this host cannot create the symlink this fixture needs")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "kb")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "kb")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatalf("copyInto over a symlink: %v", err)
	}
	fi, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("destination is still a symlink; it will dangle once the AppImage unmounts")
	}
}

// canSymlink reports whether this host lets THIS process create a symlink.
//
// It probes rather than checking runtime.GOOS because the answer is not a
// property of the OS: Windows grants SeCreateSymbolicLinkPrivilege to an
// elevated process and to any process when Developer Mode is on, and denies it
// otherwise. A GOOS check would skip on a Windows machine that can in fact
// symlink, and — worse — an elevated developer shell would report a pass for
// behaviour that fails for the users the app ships to.
func canSymlink(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		t.Logf("symlink probe failed, treating this host as unable to symlink: %v", err)
		return false
	}
	return true
}

// The bundled tools are built with the platform's executable suffix, so the
// lookup has to use it too. Without this, installBundledTool looked for
// "kb" next to a "kb.exe" and found nothing on Windows —
// and because both installers are best-effort, the app started up looking
// perfectly healthy while no MCP client could find the bridge and knomit-okf
// was never placed on the user's PATH. A warning in a log file was the only
// sign.
//
// Asserted through the error message because installBundledTool resolves
// against os.Executable(), which a test cannot redirect; the name it reports
// is the name it looked for.
func TestInstallTool_LooksForThePlatformExecutableName(t *testing.T) {
	want := map[string]string{"windows": ".exe"}[runtime.GOOS]

	for _, tc := range []struct {
		name string
		base string
		call func(string) (string, error)
	}{
		{"bridge", bridgeExecName, installBridgeTool},
		{"okf", okfExecName, installOKFTool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call(t.TempDir())
			if err == nil {
				t.Fatal("expected an error with no bundled tool present")
			}
			if !strings.Contains(err.Error(), tc.base+want) {
				t.Errorf("looked for the wrong filename on %s: want %q in %v",
					runtime.GOOS, tc.base+want, err)
			}
		})
	}
}

// The suffix must be empty everywhere but Windows — appending ".exe" on macOS
// or Linux would break the platforms that currently work.
func TestExeSuffixIsWindowsOnly(t *testing.T) {
	switch runtime.GOOS {
	case "windows":
		if exeSuffix != ".exe" {
			t.Errorf("exeSuffix = %q, want .exe", exeSuffix)
		}
	default:
		if exeSuffix != "" {
			t.Errorf("exeSuffix = %q on %s, want empty", exeSuffix, runtime.GOOS)
		}
	}
}

// replaceFile's happy path: a plain rename when nothing is in the way.
func TestReplaceFile_RenamesOverAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "kb")
	tmp := filepath.Join(dir, ".knomit-tool-new")
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceFile(tmp, dst); err != nil {
		t.Fatalf("replaceFile: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("dst = %q, want new", b)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("the staging file should have been renamed away, not copied")
	}
}

// A rename that cannot succeed for a reason moving the destination aside would
// not fix must report the ORIGINAL failure, not a confusing one from the
// fallback. Here the source does not exist at all.
func TestReplaceFile_ReportsTheRealErrorWhenThereIsNothingToMoveAside(t *testing.T) {
	dir := t.TempDir()
	err := replaceFile(filepath.Join(dir, "absent"), filepath.Join(dir, "kb"))
	if err == nil {
		t.Fatal("expected an error renaming a file that does not exist")
	}
	if strings.Contains(err.Error(), "move aside") {
		t.Errorf("reported the fallback's error rather than the real one: %v", err)
	}
}

// The debris sweep: both shapes go, and real files stay.
func TestSweepToolDebrisRemovesStagingFilesOnly(t *testing.T) {
	dir := t.TempDir()
	debris := []string{".knomit-tool-123456", ".knomit-tool-old-987654"}
	keep := []string{"kb", "knomit-okf", "something-else"}
	for _, n := range append(append([]string{}, debris...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sweepToolDebris(dir)

	for _, n := range debris {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", n)
		}
	}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was swept but must be kept: %v", n, err)
		}
	}
}

// A missing bin dir is the fresh-install case and must not panic or error.
func TestSweepToolDebrisToleratesAMissingDir(t *testing.T) {
	sweepToolDebris(filepath.Join(t.TempDir(), "nope", "bin"))
}

// The FALLBACK path, end to end: CreateTemp, rename-aside, rename-in, and the
// best-effort remove of the aside. Nothing reached it before, so the branch
// that exists specifically to replace a RUNNING kb.exe on Windows
// was never executed by a test on any platform.
//
// A non-empty directory at dst is the portable way in. os.Rename(tmp, dst)
// refuses to replace a directory that has entries, while os.Stat(dst)
// succeeds — which is exactly the shape replaceFile's fallback is written for
// (something is genuinely in the way) without needing a locked file, and so
// it runs identically on macOS, Linux and Windows.
//
// The final os.Remove of the sidelined directory fails here, because it is
// non-empty. That is the point: it proves the ignored-failure path leaves a
// correct result rather than an error, which is what happens in production
// when the sidelined binary is still running.
func TestReplaceFile_FallsBackWhenTheDestinationCannotBeRenamedOver(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "kb")
	tmp := filepath.Join(dir, ".knomit-tool-new")

	// A non-empty directory where the binary should be.
	if err := os.MkdirAll(filepath.Join(dst, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "occupied", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceFile(tmp, dst); err != nil {
		t.Fatalf("replaceFile fallback: %v", err)
	}

	// The new binary is at the name clients launch, and it is a FILE now.
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if fi.IsDir() {
		t.Fatal("dst is still a directory; the fallback did not put the new file in place")
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("dst = %q, want new", b)
	}
	// The staging file was renamed away, not copied.
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("the staging file is still present")
	}
	// And the thing that was in the way was moved aside, not destroyed.
	asides, _ := filepath.Glob(filepath.Join(dir, ".knomit-tool-old-*"))
	if len(asides) != 1 {
		t.Fatalf("expected exactly one sidelined entry, got %v", asides)
	}
	if _, err := os.Stat(filepath.Join(asides[0], "occupied", "f")); err != nil {
		t.Errorf("the sidelined content was lost: %v", err)
	}
	// sweepToolDebris is what eventually clears it — .knomit-tool-old-* is
	// matched by the same glob.
	sweepToolDebris(dir)
}

// When the fallback's final rename fails, the original must be put BACK: a
// user left with no tool at all is worse than one left with the old version.
//
// Forcing that branch takes some care. Making the STAGING path a directory
// does not work — once the aside has been moved, the destination name is free
// and renaming a directory onto a free name succeeds. What does work is a
// staging path that does not exist: the first rename fails, os.Stat(dst) still
// succeeds so the fallback runs, the aside is moved, and the rename-in then
// fails with the same missing source.
//
// A vanished staging file is artificial — copyInto has just written it — but
// the branch under test is "the rename-in failed for whatever reason", and
// this is the one way to reach it that behaves identically on every OS.
func TestReplaceFile_RestoresTheOriginalWhenTheFallbackCannotFinish(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "kb")
	missingTmp := filepath.Join(dir, ".knomit-tool-never-written")

	if err := os.MkdirAll(filepath.Join(dst, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "occupied", "f"), []byte("kb"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := replaceFile(missingTmp, dst); err == nil {
		t.Fatal("expected an error when the replacement cannot be moved into place")
	}

	// The original is back under its own name, with its own content.
	b, err := os.ReadFile(filepath.Join(dst, "occupied", "f"))
	if err != nil {
		t.Fatalf("the original was not restored: %v", err)
	}
	if string(b) != "kb" {
		t.Errorf("restored content = %q, want kb", b)
	}
}
