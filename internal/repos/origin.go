package repos

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"knomit/internal/platform/fileuri"
)

// localOriginPath reports whether s denotes a local filesystem origin and, if
// so, the absolute path go-git would actually clone from. Classification is
// delegated to go-git's own endpoint parser so the gate can never disagree with
// the cloner: go-git treats every string that is neither a scheme:// URL nor an
// scp-style host:path as a local file path (its parseFile runs filepath.Abs),
// and resolves file:// URLs to their path. This is what closes both the
// relative-path hole (e.g. "../../etc" → an absolute file path the gate now
// inspects) and the file://host divergence (go-git and the gate agree on the
// effective path). Network origins (https://, ssh://, git://, git@host:path)
// return ("", false).
func localOriginPath(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	ep, err := transport.NewEndpoint(s)
	if err != nil {
		// Unparseable as any endpoint — not a local origin we can vet. Leave it
		// to the downstream clone to surface the error rather than silently
		// treating it as an allowed network origin.
		return "", false
	}
	if ep.Protocol != "file" {
		return "", false
	}
	// go-git reaches this same path by two routes that do NOT agree on
	// Windows, and the gate has to end up where the cloner does:
	//
	//   bare path   -> parseFile, which runs filepath.Abs
	//   file: URL   -> parseURL, which hands back url.Path untouched
	//
	// On Unix that difference is invisible: url.Path for file:///srv/kb is
	// already "/srv/kb", which is what Abs returns too. On Windows the URL
	// route yields "/C:/srv/kb" — a leading slash the OS cannot use — and the
	// bare route yields "C:\srv\kb". Comparing either against the configured
	// root then fails on volume mismatch and every local origin is refused.
	//
	// The file transport strips that slash before it runs git
	// (transport/file.adjustPathForWindows); fileuri.TrimDriveSlash is that
	// same rule, and Abs finishes the job so both routes produce one absolute
	// host path.
	//
	// It works from ep.Path rather than re-parsing s, which keeps the
	// file://host behaviour this function already had: go-git's parseURL
	// discards the authority and clones u.Path, so "file://evil/srv/kb" is
	// "/srv/kb" to the cloner and must be "/srv/kb" to the gate. Recovering
	// the host here — treating it as a UNC path, say — would make the gate
	// vet a path that is not the one being cloned, which is the divergence
	// this function exists to close.
	//
	// GOOS-gated at the CALL SITE, matching go-git's adjustPathForWindows,
	// which does the same. The rule is a Windows rule: off Windows a leading
	// slash before something that looks like a drive letter is just a path,
	// and "/C:/x" is a legal (if odd) Unix filename that the gate must not
	// silently rewrite into the relative "C:/x". fileuri.TrimDriveSlash itself
	// stays unconditional — it describes file: URI syntax, which is the same
	// everywhere, and the decision about whether to APPLY it belongs here.
	p := ep.Path
	if runtime.GOOS == "windows" {
		p = fileuri.TrimDriveSlash(p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	return abs, true
}

// validateLocalOrigin enforces the local-origin policy. Network origins pass
// through untouched. Local-filesystem origins (bare absolute paths, relative
// paths, and file:// URLs — anything go-git would clone via the local file
// transport) are permitted only when localOriginRoot is configured AND the
// origin resolves to a path within that root — otherwise the server could be
// steered to clone arbitrary repos off its own disk. An empty localOriginRoot
// disables local origins entirely.
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
