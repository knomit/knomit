package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfig_RoundTripAndOmitsSourceByDefault(t *testing.T) {
	dir := t.TempDir()
	in := Config{Branch: "main", SyncedCommit: "abc123", ToolVersion: "0.5.0"}
	require.NoError(t, writeConfig(dir, in))
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "source:", "source must be absent unless --publish-source")
	out, err := readConfig(dir)
	require.NoError(t, err)
	require.Equal(t, in, out)

	missing, err := readConfig(t.TempDir())
	require.NoError(t, err, "a missing config is not an error")
	require.Empty(t, missing.Branch)
}

func TestConfig_PublishSourceIsRoundTripped(t *testing.T) {
	dir := t.TempDir()
	in := Config{Branch: "main", SyncedCommit: "abc123", Source: "https://github.com/knomit/knomit-kb"}
	require.NoError(t, writeConfig(dir, in))
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	require.NoError(t, err)
	require.Contains(t, string(raw), "source: https://github.com/knomit/knomit-kb")
	require.Contains(t, string(raw), configHeader, "the maintained-by header must be present")

	out, err := readConfig(dir)
	require.NoError(t, err)
	require.Equal(t, in, out)
}
