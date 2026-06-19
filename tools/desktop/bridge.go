//go:build desktop

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// bridgeExecName is the bundled stdio↔HTTP MCP adapter shipped next to the
// desktop binary (Contents/MacOS in the macOS bundle).
const bridgeExecName = "knomit-bridge"

// installBridgeSymlink makes the bundled knomit-bridge reachable at a stable,
// app-location-independent path (<home>/bin/knomit-bridge) so MCP client
// configs can launch it by a path that survives app moves and updates. It
// returns the link path on success.
//
// The link target is the knomit-bridge binary sitting next to the running
// executable; if it is not present — e.g. a bare `go run` during dev — the
// install is skipped with an error. Callers treat this as best-effort.
func installBridgeSymlink(home string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	// Resolve symlinks so the target is the real binary, not e.g. a prior
	// install of this very link.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	target := filepath.Join(filepath.Dir(exe), bridgeExecName)
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("bundled %s not found at %s: %w", bridgeExecName, target, err)
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
