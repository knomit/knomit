package repos

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Upstream detection no longer has a builder-side entry point: the builder
// never clones, so there is nothing to detect before a repo exists. Detection
// and its "could not reach remote HEAD" warn now live in
// store.InitFromRemote, covered by TestInitFromRemote_DetectsRemoteHEAD and
// TestInitFromRemote_PrefersMainOverAgentBranchHEAD.

// TestRecoverFromOrigin_LogsWarnOnGetRemoteError pins that a GetRemote
// failure (e.g. closed DB, corrupt remotes table) is logged at warn level
// rather than silently degrading the repo to "no origin configured". The
// prior `remote, _ := …` form would have hidden the failure and left the
// reconcile loop unstarted with no diagnostic.
func TestRecoverFromOrigin_LogsWarnOnGetRemoteError(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	// An injected origin is what makes GetRemote read the DB at all — without
	// one it short-circuits to (nil, nil) and never fails. Then close the DB so
	// the status-row read returns "database is closed".
	svc.SetOrigin(&store.Origin{URL: "https://example.test/kb.git", Branch: "main"})
	require.NoError(t, svc.Close())

	var buf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = origLogger })

	b := &repoBuilder{
		name: "alpha",
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
	// See the sibling test: only an injected origin makes GetRemote read the DB.
	svc.SetOrigin(&store.Origin{URL: "https://example.test/kb.git", Branch: "main"})
	require.NoError(t, svc.Close())

	var buf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = origLogger })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &repoBuilder{
		name: "beta",
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
