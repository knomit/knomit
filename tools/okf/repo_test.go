package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
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

// --source is documented as a one-off override, so it must NOT repoint the
// stored knomit-source remote. Fetching through the stored remote and keeping
// its URL "in step" made every one-off permanent — and with pruning enabled,
// a single sync against another knowledge base would also drop the original's
// source refs. Only `clone` decides where a repository's knowledge comes from.
func TestFetchSource_DoesNotRewriteTheStoredRemote(t *testing.T) {
	kbA, _ := newKB(t)
	kbB, _ := newKB(t) // a different knowledge base
	outDir, _ := cloneKB(t, kbA)

	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	before, err := repo.Remote(sourceRemote)
	require.NoError(t, err)
	require.Equal(t, kbA, before.Config().URLs[0])

	var buf bytes.Buffer
	require.NoError(t, runSync([]string{"--source", kbB}, outDir, &buf))

	after, err := repo.Remote(sourceRemote)
	require.NoError(t, err)
	require.Equal(t, kbA, after.Config().URLs[0],
		"a one-off --source must leave the stored remote alone")

	// A later bare sync therefore still follows the ORIGINAL knowledge base.
	cfg, err := readConfig(outDir)
	require.NoError(t, err)
	url, err := resolveSourceURL(repo, cfg, "")
	require.NoError(t, err)
	require.Equal(t, kbA, url)
}

// Threading auth must not disturb the anonymous path: a nil AuthMethod has to
// behave exactly as before, or every knomit-instance user regresses.
func TestFetchSource_NilAuthStillFetches(t *testing.T) {
	kbDir, _ := newKB(t)
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	require.NoError(t, fetchSource(repo, kbDir, nil))

	branches, err := sourceBranches(repo)
	require.NoError(t, err)
	require.NotEmpty(t, branches, "an anonymous local fetch must still populate source refs")
}

func TestOwns(t *testing.T) {
	for _, p := range []string{"index.md", "log.md", ".knomit-okf.yaml", "kb/a/b.md", "views/index.md"} {
		require.True(t, owns(p), "%s must be owned", p)
	}
	for _, p := range []string{"README.md", "LICENSE", ".github/ci.yml", "kbextra/x.md", "docs/kb/x.md"} {
		require.False(t, owns(p), "%s must NOT be owned", p)
	}
}
