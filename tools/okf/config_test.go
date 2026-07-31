package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// putConfig lays a config down on disk the way export does — through
// marshalConfig, the only sanctioned renderer — so these tests exercise the
// bytes a real sync commits rather than a test-only writer's.
func putConfig(t *testing.T, dir string, c Config) []byte {
	t.Helper()
	raw, err := marshalConfig(c)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, configFile), raw, 0o644))
	return raw
}

func TestConfig_RoundTripAndOmitsSourceByDefault(t *testing.T) {
	dir := t.TempDir()
	in := Config{Branch: "main", SyncedCommit: "abc123", ToolVersion: "0.5.0"}
	raw := putConfig(t, dir, in)
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
	raw := putConfig(t, dir, in)
	require.Contains(t, string(raw), "source: https://github.com/knomit/knomit-kb")
	require.Contains(t, string(raw), configHeader, "the maintained-by header must be present")

	out, err := readConfig(dir)
	require.NoError(t, err)
	require.Equal(t, in, out)
}

func TestReleaseOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0.5.0.78233d95", "0.5.0"}, // the injected build form
		{"0.5.0", "0.5.0"},          // no SHA injected
		{"dev", "dev"},              // a bare `go build`
		{"", ""},                    // an older config with no tool_version
		{"1.2.3.4.5", "1.2.3"},      // never more than three fields
	} {
		require.Equal(t, tc.want, releaseOf(tc.in), "releaseOf(%q)", tc.in)
	}
}
