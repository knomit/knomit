//go:build desktop

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// The CLI tools shipped next to the desktop binary (Contents/MacOS in the
// macOS bundle, the tarball root on Linux). Both are pure Go — no CGO, no
// dylibs — so they run straight from where they are staged.
const (
	// bridgeExecName is the stdio↔HTTP MCP adapter stdio clients launch.
	bridgeExecName = "knomit-bridge"
	// okfExecName is the OKF export CLI.
	okfExecName = "knomit-okf"
)

// installBridgeTool exposes the bundled knomit-bridge at a stable path so
// MCP client configs can launch it by a path that survives app moves and
// updates.
func installBridgeTool(home string) (string, error) {
	return installBundledTool(home, bridgeExecName)
}

// installOKFTool exposes the bundled knomit-okf on the same stable path.
// Without it the CLI exists only inside the app bundle, where a user would
// have to type /Applications/Knomit.app/Contents/MacOS/knomit-okf to run it.
func installOKFTool(home string) (string, error) {
	return installBundledTool(home, okfExecName)
}

// installBundledTool makes a bundled tool reachable at a stable,
// app-location-independent path (<home>/bin/<execName>). It returns the
// installed path on success.
//
// The source is the binary sitting next to the running executable; if it
// is not present — e.g. a bare `go run` during dev — the install is skipped
// with an error. Callers treat this as best-effort.
func installBundledTool(home, execName string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	// Resolve symlinks so the target is the real binary, not e.g. a prior
	// install of this very link.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	target := filepath.Join(filepath.Dir(exe), execName+exeSuffix)
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("bundled %s not found at %s: %w", execName+exeSuffix, target, err)
	}
	binDir := filepath.Join(home, "bin")
	sweepToolDebris(binDir)
	return placeTool(runtime.GOOS, binDir, target)
}

// sweepToolDebris removes staging files left behind in binDir by an
// interrupted install.
//
// Two things leave them. copyInto stages through a `.knomit-tool-*` temp file
// and removes it with a defer, which does not run if the process is killed
// mid-copy — a crash during startup leaves a partial ~40MB file. And
// replaceFile moves a locked binary to `.knomit-tool-old-*` and then fails to
// delete it precisely because it is still running.
//
// Both comments used to say this debris was "swept up by the next start". It
// was not: nothing swept, and copyInto stamps each copy with the source's
// mtime, so the leftovers were permanent. This is that sweep.
//
// Best-effort throughout. A file still held by a running process cannot be
// removed on Windows, and that is the normal case for `.knomit-tool-old-*` —
// it goes on the next start, once the old process has exited. Failing an
// install over undeletable debris would be worse than the debris.
//
// Safe to run here because installs are sequential and happen at startup,
// before any copy is in flight; a concurrent installer would need this to
// exclude the file it is currently staging.
func sweepToolDebris(binDir string) {
	matches, err := filepath.Glob(filepath.Join(binDir, ".knomit-tool-*"))
	if err != nil {
		return // only ErrBadPattern, which this pattern is not
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// exeSuffix is what the OS requires on the end of an executable's filename.
//
// The bundled tools are built as knomit-bridge.exe and knomit-okf.exe on
// Windows (the Makefile's $(EXE)), so looking for the bare name finds nothing
// and BOTH installs are skipped — with a warning, because callers treat this
// as best-effort, so the app comes up looking healthy while no MCP client can
// find knomit-bridge and `knomit-okf` is not on the user's PATH.
//
// It also has to be on the DESTINATION name, which it is: placeTool names the
// installed copy after target's base.
var exeSuffix = map[string]string{"windows": ".exe"}[runtime.GOOS]

// placeTool installs target into binDir the way goos requires. goos is a
// parameter rather than a runtime.GOOS read so both branches are testable on
// either host — the Linux branch would otherwise never execute in CI on macOS.
//
// Linux ships as an AppImage, whose contents live in a FUSE mount torn down
// when the app exits. A symlink into it would dangle the moment the app quits,
// breaking every MCP client wired to <home>/bin, so the tool is COPIED.
//
// Windows COPIES as well, for a different reason: creating a symlink there
// needs SeCreateSymbolicLinkPrivilege, which an ordinary user does not have
// unless Developer Mode is switched on. linkInto would fail outright for most
// users, and "most users" is not a case to leave to a runtime error.
//
// macOS keeps the symlink: the .app sits at a stable path and the updater
// replaces the bundle in place, so the link stays valid and the bundled tools
// update along with the app.
func placeTool(goos, binDir, target string) (string, error) {
	if goos == "linux" || goos == "windows" {
		return copyInto(binDir, target)
	}
	return linkInto(binDir, target)
}

// linkInto creates (or refreshes) a symlink to target inside binDir, named
// after target's base. It is idempotent: an existing link already pointing at
// target is left alone; any other existing entry at the link path (stale link,
// regular file) is replaced. Returns the link path.
func linkInto(binDir, target string) (string, error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", binDir, err)
	}
	link := filepath.Join(binDir, filepath.Base(target))
	if current, err := os.Readlink(link); err == nil && current == target {
		return link, nil // already correct
	}
	// Remove whatever is there (wrong link, or a regular file); ignore a
	// not-exist error so a fresh install works.
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale %s: %w", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		return "", fmt.Errorf("symlink %s -> %s: %w", link, target, err)
	}
	return link, nil
}

// copyInto copies target into binDir under target's base name, replacing
// whatever is already there. Unlike linkInto it always produces a real file,
// so the result outlives the source it was copied from — which is the whole
// point under an AppImage, where the source lives in a FUSE mount that is
// unmounted when the app exits.
//
// The copy is staged to a temp file, flushed, and renamed, so neither a crash
// nor a power loss mid-write can leave a truncated binary at the destination,
// and replacing a file that a running MCP client still holds open is safe: the
// open fd keeps the old inode alive while new launches get the new one.
//
// It is also a no-op when the destination already matches — see upToDate. This
// runs on every app start, so without that check each launch would decompress
// and rewrite both bundled tools out of the AppImage's squashfs image for
// nothing.
func copyInto(binDir, target string) (string, error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", binDir, err)
	}
	dst := filepath.Join(binDir, filepath.Base(target))

	srcInfo, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", target, err)
	}
	// Lstat, not Stat: a symlink from an older tarball install resolves
	// through Stat to whatever it points at, which could match by coincidence
	// and leave the dangling link in place — the exact bug this file exists
	// to fix.
	if dstInfo, lerr := os.Lstat(dst); lerr == nil && upToDate(srcInfo, dstInfo) {
		return dst, nil
	}

	src, err := os.Open(target)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", target, err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp(binDir, ".knomit-tool-*")
	if err != nil {
		return "", fmt.Errorf("staging file in %s: %w", binDir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return "", fmt.Errorf("copy %s: %w", target, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	// Rename is atomic against a crash, but not against a power loss: the
	// directory entry can reach disk before the data does, leaving a
	// zero-length binary at a path MCP clients launch by name.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("flush %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", tmpName, err)
	}
	// Carry the source's mtime across BEFORE the rename, so it lands with the
	// file rather than in a window after it. This is what upToDate matches on
	// the next launch; without it every copy looks new and nothing is ever
	// skipped.
	if err := os.Chtimes(tmpName, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		return "", fmt.Errorf("set mtime on %s: %w", tmpName, err)
	}
	// Rename replaces a regular file atomically, but writing through a symlink
	// left by a previous tarball install would corrupt whatever it points at —
	// remove any symlink at the destination first.
	if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dst); err != nil {
			return "", fmt.Errorf("remove stale symlink %s: %w", dst, err)
		}
	}
	if err := replaceFile(tmpName, dst); err != nil {
		return "", fmt.Errorf("install %s: %w", dst, err)
	}
	return dst, nil
}

// replaceFile renames tmpName over dst, falling back to moving dst aside first.
//
// On Unix the plain rename always works: unlinking a file that a process has
// open is legal, and the open fd keeps the old inode alive. On WINDOWS it is
// not. A file that is mapped as a running executable cannot be deleted or
// overwritten, so the rename fails with a sharing violation whenever the user
// has an MCP client holding knomit-bridge.exe open — which, since the client
// is the reason the tool is installed, is the common case rather than the
// exotic one.
//
// Windows does allow RENAMING such a file. So: move the old one aside, put the
// new one in its place, and try to delete the sidelined copy. The delete fails
// while the process is still running; that is fine and deliberately ignored —
// the name every MCP client launches now points at the new binary, and the
// leftover is swept up by the next start, once the old process has exited.
//
// The sidelined name stays inside binDir so the move is a same-volume rename
// rather than a copy, and is prefixed to match what the temp-file sweep
// already recognises as debris.
func replaceFile(tmpName, dst string) error {
	err := os.Rename(tmpName, dst)
	if err == nil {
		return nil
	}
	if _, serr := os.Stat(dst); serr != nil {
		// Nothing is in the way, so moving it aside cannot be the fix — the
		// rename failed for some other reason. Report that one.
		return err
	}

	aside, aerr := os.CreateTemp(filepath.Dir(dst), ".knomit-tool-old-*")
	if aerr != nil {
		return fmt.Errorf("stage replacement of %s: %w", dst, aerr)
	}
	asideName := aside.Name()
	if cerr := aside.Close(); cerr != nil {
		return fmt.Errorf("stage replacement of %s: %w", dst, cerr)
	}
	// CreateTemp made the file; Rename needs the name free on Windows.
	if rerr := os.Remove(asideName); rerr != nil {
		return fmt.Errorf("stage replacement of %s: %w", dst, rerr)
	}
	if rerr := os.Rename(dst, asideName); rerr != nil {
		return fmt.Errorf("move aside %s: %w", dst, rerr)
	}
	if rerr := os.Rename(tmpName, dst); rerr != nil {
		// Put it back rather than leaving the user with no tool at all.
		_ = os.Rename(asideName, dst)
		return rerr
	}
	_ = os.Remove(asideName) // still running: swept up on a later start
	return nil
}

// upToDate reports whether dst is already the copy copyInto would produce.
//
// Size plus mtime, the rsync quick check. It is sound HERE specifically
// because copyInto stamps each copy with its source's mtime and the source is
// a read-only file inside an AppImage: a matching pair means the bytes came
// from this exact build. An update ships a new AppImage with a fresh mtime, so
// the copy is redone.
//
// dst must be a REGULAR file. A symlink that happens to satisfy the size and
// time comparison still has to be replaced — it points into a FUSE mount that
// disappears when the app exits, which is the failure this whole path exists
// to prevent.
func upToDate(src, dst os.FileInfo) bool {
	return dst.Mode().IsRegular() &&
		src.Size() == dst.Size() &&
		src.ModTime().Equal(dst.ModTime())
}
