package repos

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestResolveOriginUpstream_LogsWarnAndDefaultsOnFailure regression-tests PR
// #61 review finding #2: when the remote's symbolic HEAD cannot be reached
// (bad token, unreachable URL, etc.), the builder used to silently fall back
// to "main" with no log output. An operator whose origin is on `master` but
// whose ls-remote failed would see a "no commits" repo with no diagnostic.
//
// resolveOriginUpstream now (a) returns "main" as fallback and (b) emits a
// warn-level log when detection fails — making the misconfiguration visible.
func TestResolveOriginUpstream_LogsWarnAndDefaultsOnFailure(t *testing.T) {
	var buf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = origLogger })

	b := &repoBuilder{
		name: "alpha",
		cfg:  config.Config{Git: config.GitConfig{Origin: "file:///nonexistent-knomit-test-dir-xyz"}},
	}

	got := b.resolveOriginUpstream(nil)
	require.Equal(t, "main", got, "must fall back to \"main\" when detection fails")

	logged := buf.String()
	require.NotEmpty(t, logged, "expected a warn log when detection fails; got nothing")

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(logged)), &entry),
		"warn log must be a single JSON entry; got: %s", logged)
	require.Equal(t, "warn", entry["level"])
	require.Equal(t, "alpha", entry["repo"])
}
