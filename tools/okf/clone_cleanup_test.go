package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// bogusKBURL is a path that is not a git repository, so the fetch fails after
// the directory has already been created and initialised.
func bogusKBURL(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "not-a-repo")
}

// A clone that dies partway used to leave a half-built repository that
// ensureEmptyDir then refused, so the obvious retry — the same command with a
// fixed token — failed with "is not empty" and demanded a manual rm -rf.
func TestClone_FailureRemovesADirectoryItCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kb-export")
	var buf bytes.Buffer

	require.Error(t, runClone([]string{bogusKBURL(t), dir}, &buf))
	require.NoDirExists(t, dir, "a directory clone created must not survive its failure")
}

// The mirror case, and the reason cleanup cannot simply RemoveAll: a directory
// the USER made is theirs. It is emptied back to how it was found, not deleted.
func TestClone_FailureEmptiesButKeepsAPreexistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mine")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	var buf bytes.Buffer

	require.Error(t, runClone([]string{bogusKBURL(t), dir}, &buf))
	require.DirExists(t, dir, "a directory the user created stays theirs")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "but it is restored to the empty state it was accepted in")
}

// The consequence that actually bit users: after a failure, retrying works.
func TestClone_RetryAfterFailureSucceeds(t *testing.T) {
	kbDir, _ := newKB(t)
	dir := filepath.Join(t.TempDir(), "kb-export")
	var buf bytes.Buffer

	require.Error(t, runClone([]string{bogusKBURL(t), dir}, &buf))
	buf.Reset()
	require.NoError(t, runClone([]string{kbDir, dir}, &buf),
		"the same command must work once the source is right")
	require.FileExists(t, filepath.Join(dir, "index.md"))
}

// A successful clone must obviously not be cleaned up.
func TestClone_SuccessIsNotCleanedUp(t *testing.T) {
	kbDir, _ := newKB(t)
	dir := filepath.Join(t.TempDir(), "kb-export")
	var buf bytes.Buffer
	require.NoError(t, runClone([]string{kbDir, dir}, &buf))
	require.FileExists(t, filepath.Join(dir, "index.md"))
	require.DirExists(t, filepath.Join(dir, ".git"))
}
