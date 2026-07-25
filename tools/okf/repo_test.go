package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconcile_RemovesStaleAndSparesPublisherFiles(t *testing.T) {
	dir := t.TempDir()
	// A publisher's own files, plus a stale bundle file from a previous sync.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("mine"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github/ci.yml"), []byte("mine"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "kb/old"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kb/old/retired.md"), []byte("stale"), 0o644))

	_, deleted, err := reconcile(dir, map[string][]byte{
		"index.md":       []byte("---\nokf_version: \"0.2\"\n---\n"),
		"kb/new/fact.md": []byte("---\ntype: decision\n---\n"),
	})
	require.NoError(t, err)
	require.Positive(t, deleted, "a retired fact's document must be removed, not left behind")

	require.NoFileExists(t, filepath.Join(dir, "kb/old/retired.md"))
	require.FileExists(t, filepath.Join(dir, "kb/new/fact.md"))
	require.FileExists(t, filepath.Join(dir, "README.md"), "publisher files must survive")
	require.FileExists(t, filepath.Join(dir, ".github/ci.yml"), "publisher files must survive")

	// The emptied directory goes too, so a retired category leaves no husk.
	require.NoDirExists(t, filepath.Join(dir, "kb/old"))
}

// A path outside the owned set is a programming error, not something to
// silently write: the owned-path boundary is what protects a publisher's repo.
func TestReconcile_RejectsUnownedPaths(t *testing.T) {
	dir := t.TempDir()
	_, _, err := reconcile(dir, map[string][]byte{"README.md": []byte("nope")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside the owned paths")
}

func TestOwns(t *testing.T) {
	for _, p := range []string{"index.md", "log.md", ".knomit-okf.yaml", "kb/a/b.md", "views/index.md"} {
		require.True(t, owns(p), "%s must be owned", p)
	}
	for _, p := range []string{"README.md", "LICENSE", ".github/ci.yml", "kbextra/x.md", "docs/kb/x.md"} {
		require.False(t, owns(p), "%s must NOT be owned", p)
	}
}
