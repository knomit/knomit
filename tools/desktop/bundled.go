//go:build desktop

package main

import (
	"fmt"
	"os"
	"path/filepath"
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

// installBridgeSymlink exposes the bundled knomit-bridge at a stable path so
// MCP client configs can launch it by a path that survives app moves and
// updates.
func installBridgeSymlink(home string) (string, error) {
	return installBundledTool(home, bridgeExecName)
}

// installOKFSymlink exposes the bundled knomit-okf on the same stable path.
// Without it the CLI exists only inside the app bundle, where a user would
// have to type /Applications/Knomit.app/Contents/MacOS/knomit-okf to run it.
func installOKFSymlink(home string) (string, error) {
	return installBundledTool(home, okfExecName)
}

// installBundledTool makes a bundled tool reachable at a stable,
// app-location-independent path (<home>/bin/<execName>). It returns the link
// path on success.
//
// The link target is the binary sitting next to the running executable; if it
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
	return linkInto(filepath.Join(home, "bin"), target)
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
