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
	target := filepath.Join(filepath.Dir(exe), execName)
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("bundled %s not found at %s: %w", execName, target, err)
	}
	return placeTool(runtime.GOOS, filepath.Join(home, "bin"), target)
}

// placeTool installs target into binDir the way goos requires. goos is a
// parameter rather than a runtime.GOOS read so both branches are testable on
// either host — the Linux branch would otherwise never execute in CI on macOS.
//
// Linux ships as an AppImage, whose contents live in a FUSE mount torn down
// when the app exits. A symlink into it would dangle the moment the app quits,
// breaking every MCP client wired to <home>/bin, so the tool is COPIED.
//
// macOS keeps the symlink: the .app sits at a stable path and the updater
// replaces the bundle in place, so the link stays valid and the bundled tools
// update along with the app.
func placeTool(goos, binDir, target string) (string, error) {
	if goos == "linux" {
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
// The copy is staged to a temp file and renamed, so a crash mid-write cannot
// leave a truncated binary at the destination, and replacing a file that a
// running MCP client still holds open is safe: the open fd keeps the old inode
// alive while new launches get the new one.
func copyInto(binDir, target string) (string, error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", binDir, err)
	}
	dst := filepath.Join(binDir, filepath.Base(target))

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
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", tmpName, err)
	}
	// Rename replaces a regular file atomically, but writing through a symlink
	// left by a previous tarball install would corrupt whatever it points at —
	// remove any symlink at the destination first.
	if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dst); err != nil {
			return "", fmt.Errorf("remove stale symlink %s: %w", dst, err)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("install %s: %w", dst, err)
	}
	return dst, nil
}
