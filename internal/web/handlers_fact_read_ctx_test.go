package web

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/web/hal"
	"knomit/test/testenv"
)

// seedCtxTestRepo builds a real store-backed repo with one fact, so the
// cancellation anchors below exercise the production provider against the
// production store rather than a stub that could pass by construction.
func seedCtxTestRepo(t *testing.T) (*testenv.RepoHandle, string) {
	t.Helper()
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("ctxkb")
	const path = "kb/architecture/ctx.md"
	r.Branch("main").Write(path, testenv.Fact("ctx anchor").Body("body"), "seed")
	return r, path
}

// TestDefaultFactReader_Read_CancelledContext pins that the web provider layer
// actually honours the caller's context. Before ctx threading the provider used
// contextTODO() (== context.Background()), so a cancelled request context was
// invisible to the store and the read succeeded anyway — a client that hung up
// still paid for the full query. The read must now fail with the caller's
// cancellation instead of returning data nobody is waiting for.
func TestDefaultFactReader_Read_CancelledContext(t *testing.T) {
	r, path := seedCtxTestRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := defaultFactReader{}.Read(ctx, r.Instance(), hal.Anchor{Branch: "main"}, path, false)
	require.Error(t, err, "a cancelled request context must not yield a successful read")
	require.ErrorIs(t, err, context.Canceled)
}

// TestDefaultFactReader_Exists_CancelledContextLogsDebug pins the log-level
// refinement that ctx threading makes necessary. Exists collapses every error
// into "broken" and logs at ERROR so transient DB faults stay observable — but
// once the request context reaches FactExistsAt, an ordinary client disconnect
// mid-render becomes an error, and every abandoned page would emit ERROR spam
// for each ref it was resolving. Cancellation is expected, not exceptional, so
// it logs at DEBUG; everything else keeps its ERROR.
func TestDefaultFactReader_Exists_CancelledContextLogsDebug(t *testing.T) {
	r, path := seedCtxTestRepo(t)

	// Capture the global logger at DEBUG so both an offending ERROR and the
	// intended DEBUG land in the buffer (the repo's usual capture idiom).
	var buf bytes.Buffer
	origLogger, origLevel := log.Logger, zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() {
		log.Logger = origLogger
		zerolog.SetGlobalLevel(origLevel)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	exists := defaultFactReader{}.Exists(ctx, r.Instance(), "main", path, "")
	require.False(t, exists, "a cancelled lookup cannot claim the ref exists")

	out := buf.String()
	require.NotContains(t, out, `"level":"error"`,
		"cancellation is a routine client disconnect, not an error worth alerting on; got:\n"+out)
	require.True(t,
		strings.Contains(out, `"level":"debug"`) || out == "",
		"expected the cancellation to be recorded at DEBUG (or not at all); got:\n"+out)
}
