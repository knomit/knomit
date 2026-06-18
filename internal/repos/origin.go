package repos

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// localOriginPath reports whether s denotes a local filesystem origin — a bare
// absolute path or a file:// URL — and returns the path it refers to. Every
// other remote shape (https://, ssh://, git://, scp-style git@host:path)
// returns ("", false).
func localOriginPath(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		// file:///srv/kb → /srv/kb. Tolerate a host component by preferring
		// the parsed Path; fall back to the raw remainder if parsing fails.
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			return u.Path, true
		}
		return rest, true
	}
	if filepath.IsAbs(s) {
		return s, true
	}
	return "", false
}

// validateLocalOrigin enforces the local-origin policy. Network origins pass
// through untouched. Local-filesystem origins (bare absolute paths or file://
// URLs) are permitted only when localOriginRoot is configured AND the origin
// resolves to a path within that root — otherwise the server could be steered
// to clone arbitrary repos off its own disk. An empty localOriginRoot disables
// local origins entirely.
//
// Containment is checked against the symlink-resolved real paths, not the
// lexical ones: go-git follows symlinks when cloning a local path, so a symlink
// living inside the root that points outside it would otherwise escape the gate
// (TestValidateLocalOrigin_SymlinkEscape). A residual TOCTOU window remains —
// the path could be re-pointed between this check and the clone — but resolving
// here closes the static-symlink hole.
func validateLocalOrigin(s, localOriginRoot string) error {
	path, ok := localOriginPath(s)
	if !ok {
		return nil
	}
	if localOriginRoot == "" {
		return fmt.Errorf("local-path origins are disabled — set local_origin_root (or KNOMIT_LOCAL_ORIGIN_ROOT) to allow them")
	}
	root := resolveSymlinks(filepath.Clean(localOriginRoot))
	target := resolveSymlinks(filepath.Clean(path))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("local origin %q is outside the allowed root %q", path, localOriginRoot)
	}
	return nil
}

// resolveSymlinks returns p with all symlink components resolved. filepath.Eval-
// Symlinks requires the path to exist, so for an origin that does not exist yet
// it resolves the longest existing prefix and re-appends the remainder — this
// follows an intermediate symlink (e.g. <root>/link/repo where <root>/link is a
// symlink) even when the leaf is absent.
func resolveSymlinks(p string) string {
	p = filepath.Clean(p)
	rem := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rem)
		}
		parent := filepath.Dir(p)
		if parent == p {
			// Reached the filesystem root without resolving anything.
			return filepath.Join(p, rem)
		}
		rem = filepath.Join(filepath.Base(p), rem)
		p = parent
	}
}

// ValidateLocalOrigin reports whether the given origin URL is permitted under
// this manager's local-origin policy (config.LocalOriginRoot). Network origins
// always pass; local-filesystem origins are gated. It is the single enforcement
// point the web layer calls before persisting an origin whose clone is deferred
// to the sync loop; the immediate-clone paths are gated inside ResolveAuth.
func (m *Manager) ValidateLocalOrigin(originURL string) error {
	return validateLocalOrigin(originURL, m.deps.Cfg.LocalOriginRoot)
}
