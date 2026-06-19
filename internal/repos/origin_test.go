package repos

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestValidateLocalOrigin covers the local-origin policy gate: network origins
// always pass; local origins (bare absolute paths and file:// URLs) are allowed
// only when a root is configured AND the path resolves within it. An empty root
// disables local origins entirely.
func TestValidateLocalOrigin(t *testing.T) {
	cases := []struct {
		name string
		url  string
		root string
		ok   bool
	}{
		// Network origins are never gated, regardless of root.
		{"https no root", "https://github.com/user/repo.git", "", true},
		{"ssh no root", "git@github.com:user/repo.git", "", true},
		{"ssh scheme no root", "ssh://git@github.com/user/repo.git", "", true},

		// Local origins are rejected when no root is configured.
		{"bare abs disabled", "/srv/kb", "", false},
		{"file url disabled", "file:///srv/kb", "", false},

		// Local origins within the configured root are allowed.
		{"bare abs within root", "/srv/kb/work", "/srv/kb", true},
		{"bare abs equal root", "/srv/kb", "/srv/kb", true},
		{"file url within root", "file:///srv/kb/work", "/srv/kb", true},

		// Local origins outside the root are rejected (incl. traversal).
		{"bare abs outside root", "/etc/passwd", "/srv/kb", false},
		{"file url outside root", "file:///etc/passwd", "/srv/kb", false},
		{"traversal escape", "/srv/kb/../etc", "/srv/kb", false},
		{"sibling prefix not contained", "/srv/kb-evil", "/srv/kb", false},

		// Relative paths are local origins too: go-git resolves them against the
		// server cwd via filepath.Abs, so they must be gated, never waved through
		// as "network". With no root they are disabled; with a root they resolve
		// (relative to cwd) outside it and are rejected. Regression for the
		// create-path bypass where the handler never called isGitURL.
		{"relative disabled", "../../etc", "", false},
		{"relative bare disabled", "some/repo", "", false},
		{"relative outside root", "../../etc", "/srv/kb", false},

		// A relative (misconfigured) root cannot contain an absolute path.
		{"relative root rejects", "/srv/kb", "relative/root", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateLocalOrigin(c.url, c.root)
			if c.ok && err != nil {
				t.Errorf("validateLocalOrigin(%q, %q) = %v, want nil", c.url, c.root, err)
			}
			if !c.ok && err == nil {
				t.Errorf("validateLocalOrigin(%q, %q) = nil, want error", c.url, c.root)
			}
		})
	}
}

// TestValidateLocalOrigin_SymlinkEscape is the regression test for the lexical
// containment hole: a symlink *inside* the configured root that points outside
// it must not let an origin escape. go-git follows symlinks when cloning a local
// path, so a purely lexical filepath.Rel check would clone the symlink's real
// target (arbitrary on-disk content). The gate must resolve symlinks first.
func TestValidateLocalOrigin_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir() // a sibling the caller must not be able to reach

	// A symlink living inside the root that points outside it.
	escape := filepath.Join(root, "escape")
	require.NoError(t, os.Symlink(outside, escape))

	// Make the symlink target a real, clonable directory so EvalSymlinks
	// resolves the full path (mirrors a real clone of an existing repo).
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "repo"), 0o755))

	// Lexically "/root/escape/repo" looks contained, but it resolves outside.
	via := filepath.Join(escape, "repo")
	require.Error(t, validateLocalOrigin(via, root),
		"symlink inside root pointing out must be rejected")

	// A real subdirectory of the root (no symlink escape) still passes.
	inside := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	require.NoError(t, validateLocalOrigin(inside, root),
		"genuine path within root must be allowed")
}

// TestManager_ValidateLocalOrigin verifies the Manager method applies the
// gate using its own configured LocalOriginRoot.
func TestManager_ValidateLocalOrigin(t *testing.T) {
	root := t.TempDir()
	m := New(t.Context(), Deps{Cfg: config.Config{LocalOriginRoot: root}})

	require.NoError(t, m.ValidateLocalOrigin("https://github.com/user/repo.git"))
	require.NoError(t, m.ValidateLocalOrigin(filepath.Join(root, "kb")))
	require.Error(t, m.ValidateLocalOrigin("/etc/passwd"))

	// A manager with no root configured disables local origins entirely.
	off := New(t.Context(), Deps{Cfg: config.Config{}})
	require.Error(t, off.ValidateLocalOrigin("/srv/kb"))
	require.NoError(t, off.ValidateLocalOrigin("https://github.com/user/repo.git"))
}
