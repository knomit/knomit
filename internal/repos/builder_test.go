package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
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

// TestRecoverFromOrigin_LogsWarnOnGetRemoteError pins that a GetRemote
// failure (e.g. closed DB, corrupt remotes table) is logged at warn level
// rather than silently degrading the repo to "no origin configured". The
// prior `remote, _ := …` form would have hidden the failure and left the
// reconcile loop unstarted with no diagnostic.
func TestRecoverFromOrigin_LogsWarnOnGetRemoteError(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	// Close the DB so GetRemote returns a "database is closed" error.
	require.NoError(t, svc.Close())

	var buf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = origLogger })

	b := &repoBuilder{
		name: "alpha",
		cfg:  config.Config{Git: config.GitConfig{Origin: "https://example.invalid/repo.git"}},
		svc:  svc,
		ctx:  context.Background(),
	}
	b.recoverFromOrigin()

	logged := buf.String()
	require.NotEmpty(t, logged, "expected a warn log when GetRemote fails; got nothing")
	require.Contains(t, logged, "recoverFromOrigin", "log must identify the call site")
	require.Contains(t, logged, "\"repo\":\"alpha\"")
}

// TestStartSyncLoops_LogsWarnOnGetRemoteError mirrors the above for the
// background-loop entry point. A GetRemote failure here must be visible —
// otherwise the operator sees a quiet repo with no reconcile loop and no
// diagnostic.
func TestStartSyncLoops_LogsWarnOnGetRemoteError(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	require.NoError(t, svc.Close())

	var buf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = origLogger })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &repoBuilder{
		name: "beta",
		cfg:  config.Config{Git: config.GitConfig{Origin: "https://example.invalid/repo.git"}},
		svc:  svc,
		ctx:  ctx,
	}
	var wg sync.WaitGroup
	hub := NewTaskHub(ctx)
	b.startSyncLoops(ctx, &wg, hub)
	wg.Wait()

	logged := buf.String()
	require.NotEmpty(t, logged, "expected a warn log when GetRemote fails; got nothing")
	require.Contains(t, logged, "startSyncLoops", "log must identify the call site")
	require.Contains(t, logged, "\"repo\":\"beta\"")
}
