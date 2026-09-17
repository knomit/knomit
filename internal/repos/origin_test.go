package repos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/platform/fileuri"
)

// TestValidateLocalOrigin covers the local-origin policy gate: network origins
// always pass; local origins (bare absolute paths and file:// URLs) are allowed
// only when a root is configured AND the path resolves within it. An empty root
// disables local origins entirely.
func TestValidateLocalOrigin(t *testing.T) {
	// The fixtures used to be Unix path literals. "/srv/kb" is not absolute on
	// Windows — it is relative to the current drive — so filepath.Abs, which is
	// what go-git applies to a bare-path origin, turned the ORIGIN into
	// "C:\srv\kb" while the ROOT stayed "\srv\kb". Different volumes, no
	// containment, and every allowed row failed for a reason the test is not
	// about. Spelling both sides through the same Abs keeps one table honest on
	// every OS.
	var (
		kb      = hostAbs(t, "/srv/kb")
		kbWork  = filepath.Join(kb, "work")
		kbEvil  = hostAbs(t, "/srv/kb-evil")
		outside = hostAbs(t, "/etc/passwd")
		// Deliberately NOT filepath.Join, which would Clean the ".." away
		// before the gate ever sees it. Traversal has to reach validate-
		// LocalOrigin intact for this row to test anything.
		traversal = kb + string(filepath.Separator) + ".." + string(filepath.Separator) + "etc"
	)

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
		{"bare abs disabled", kb, "", false},
		{"file url disabled", fileuri.New(kb), "", false},

		// Local origins within the configured root are allowed.
		{"bare abs within root", kbWork, kb, true},
		{"bare abs equal root", kb, kb, true},
		// The regression row for the Windows gate. go-git's parseURL hands
		// back "/C:/srv/kb/work" for this URL — with a leading slash the OS
		// cannot use — while the bare-path row above went through
		// filepath.Abs and came out "C:\srv\kb\work". Without the matching
		// normalisation in localOriginPath the two disagree on volume, Rel
		// fails, and this row is REJECTED: every local file: origin refused on
		// Windows, with an error blaming the user's local_origin_root.
		{"file url within root", fileuri.New(kbWork), kb, true},

		// Local origins outside the root are rejected (incl. traversal).
		{"bare abs outside root", outside, kb, false},
		{"file url outside root", fileuri.New(outside), kb, false},
		{"traversal escape", traversal, kb, false},
		{"sibling prefix not contained", kbEvil, kb, false},

		// Relative paths are local origins too: go-git resolves them against the
		// server cwd via filepath.Abs, so they must be gated, never waved through
		// as "network". With no root they are disabled; with a root they resolve
		// (relative to cwd) outside it and are rejected. Regression for the
		// create-path bypass where the handler never called isGitURL.
		{"relative disabled", "../../etc", "", false},
		{"relative bare disabled", "some/repo", "", false},
		{"relative outside root", "../../etc", kb, false},

		// A relative (misconfigured) root cannot contain an absolute path.
		{"relative root rejects", kb, filepath.Join("relative", "root"), false},
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
	// Skipped on the CAPABILITY, not the OS. This is the regression test for a
	// security hole, and "symlink semantics differ on Windows" was hiding
	// behaviour that is in fact correct there: EvalSymlinks resolves a
	// directory symlink on Windows too, and Rel then places the target outside
	// the root exactly as it does on Unix. A GOOS skip meant the gate went
	// unverified on Windows for no reason.
	//
	// What genuinely varies is whether the PROCESS may create a symlink —
	// Windows needs SeCreateSymbolicLinkPrivilege or Developer Mode — and that
	// is about building the fixture, not about the gate.
	if !canSymlink(t) {
		t.Skip("this host cannot create the symlink this fixture needs (Windows without Developer Mode or an elevated shell)")
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
	require.Error(t, m.ValidateLocalOrigin(hostAbs(t, "/etc/passwd")))

	// A manager with no root configured disables local origins entirely.
	off := New(t.Context(), Deps{Cfg: config.Config{}})
	require.Error(t, off.ValidateLocalOrigin(hostAbs(t, "/srv/kb")))
	require.NoError(t, off.ValidateLocalOrigin("https://github.com/user/repo.git"))
}

// A file: URL with an AUTHORITY must be gated on the path go-git will
// actually clone. go-git's parseURL discards the authority and uses u.Path, so
// "file://evil/srv/kb" clones "/srv/kb"; a gate that instead recovered the
// host — reading it as a UNC path, or folding it into the path — would vet a
// location that is not the one being fetched.
//
// This had no test, only a sentence in localOriginPath's doc comment naming
// the "file://host divergence" as closed. It is pinned here because the
// Windows work reached for a full URI-to-path conversion, which does recover
// the host, and would have reopened it silently.
func TestValidateLocalOrigin_AuthorityIsIgnoredLikeGoGitDoes(t *testing.T) {
	kb := hostAbs(t, "/srv/kb")

	// Same path, spelled with a junk authority: still inside the root.
	//
	// Built by splicing the host into a well-formed URI rather than by
	// concatenating "file://evil" with the path. On Windows the path starts
	// "C:/…", so the naive form produces "file://evilC:/srv/…" — host
	// "evilC:", which is a parse error, not the case this test is about. Going
	// through fileuri.New keeps the authority and the path separate on both
	// platforms, so the drive-letter path actually reaches TrimDriveSlash.
	withAuthority := strings.Replace(fileuri.New(filepath.Join(kb, "work")), "file://", "file://evil", 1)
	require.NoError(t, validateLocalOrigin(withAuthority, kb),
		"the authority is not part of the path go-git clones, so it must not move the origin out of the root")

	// And the gate resolves it to the same place the cloner would.
	got, ok := localOriginPath(withAuthority)
	require.True(t, ok, "a file: URL with an authority is still a local origin")
	require.Equal(t, filepath.Join(kb, "work"), got,
		"the host must not appear in the resolved path")

	// The mirror: an authority cannot smuggle a path INTO the root either.
	outside := strings.Replace(fileuri.New(hostAbs(t, "/etc/passwd")), "file://", "file://evil", 1)
	require.Error(t, validateLocalOrigin(outside, kb))
}

// hostAbs spells a Unix-shaped fixture path the way the HOST does, so one
// fixture serves every OS: "/srv/kb" is absolute on Unix and drive-relative on
// Windows, where it becomes "C:\srv\kb".
//
// filepath.Abs specifically, because that is what go-git's parseFile applies
// to a bare-path origin. Picking a drive any other way would put the fixture
// on a different volume from the origin under test, and the gate would refuse
// it for a reason unrelated to the policy being checked.
//
// None of these paths is created on disk; the gate is lexical plus a symlink
// resolution that leaves a non-existent path alone.
func hostAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.FromSlash(p))
	require.NoError(t, err)
	return abs
}

// canSymlink reports whether this host lets THIS process create a symlink.
//
// A probe rather than a runtime.GOOS check, because the answer is not a
// property of the OS: Windows grants SeCreateSymbolicLinkPrivilege to an
// elevated process and to any process with Developer Mode on, and denies it
// otherwise. Checking GOOS would skip on a Windows machine that can in fact
// symlink — which is how the escape test above went unverified there.
func canSymlink(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		t.Logf("symlink probe failed, treating this host as unable to symlink: %v", err)
		return false
	}
	return true
}
