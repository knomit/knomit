package repos

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// TestReconcileLoop_UnreadableRemoteIsLoggedAndSkipped pins that a GetRemote
// failure (a closed DB, a corrupt remotes table) is logged at warn level and
// costs ticks, never the loop: the loop the Sync stage started keeps running
// until its ctx ends. It used to exit there — "initial remote read failed;
// not starting" — which left a quiet repo with no reconcile loop until the
// next restart.
func TestReconcileLoop_UnreadableRemoteIsLoggedAndSkipped(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	// An injected origin is what makes GetRemote read the DB at all — without
	// one it short-circuits to (nil, nil) and never fails. Then close the DB so
	// the status-row read returns "database is closed".
	svc.SetOrigin(&store.Origin{URL: "https://example.test/kb.git", Branch: "main"})
	require.NoError(t, svc.Close())

	var buf syncBuf
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = origLogger })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReconcileLoop(ctx, svc, NewTaskHub(ctx), "beta", "agent/x", nil, "", false, nil, nil, nil, nil, nil, false)
	}()

	require.Eventually(t, func() bool {
		return bytes.Contains([]byte(buf.String()), []byte("initial remote read failed"))
	}, 5*time.Second, 5*time.Millisecond, "expected a warn log when GetRemote fails")
	require.Contains(t, buf.String(), "\"repo\":\"beta\"")
	select {
	case <-done:
		t.Fatal("the loop exited on an unreadable remote; it must skip ticks until its ctx ends")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	<-done
}
