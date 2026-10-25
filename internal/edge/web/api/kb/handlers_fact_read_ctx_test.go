package kb

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/edge/web/hal"
	"knomit/test/testenv"
)

// seedCtxTestRepo builds a real store-backed repo with one fact, so the
// cancellation anchors below exercise the production read against the
// production store rather than a stub that could pass by construction.
func seedCtxTestRepo(t *testing.T) (*testenv.RepoHandle, string) {
	t.Helper()
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("ctxkb")
	const path = "kb/architecture/ctx.md"
	r.Branch("main").Write(path, testenv.Fact("ctx anchor").Body("body"), "seed")
	return r, path
}

// TestReadFact_CancelledContext pins that the web read layer actually honours
// the caller's context. Before ctx threading the read used
// contextTODO() (== context.Background()), so a cancelled request context was
// invisible to the store and the read succeeded anyway — a client that hung up
// still paid for the full query. The read must now fail with the caller's
// cancellation instead of returning data nobody is waiting for.
func TestReadFact_CancelledContext(t *testing.T) {
	r, path := seedCtxTestRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := readFact(ctx, r.Instance(), hal.Anchor{Branch: "main"}, path, false)
	require.Error(t, err, "a cancelled request context must not yield a successful read")
	require.ErrorIs(t, err, context.Canceled)
}

// TestReaderRefResolver_Exists_CancelledContextLogsDebug pins the log-level
// refinement that ctx threading makes necessary. Exists collapses every error
// into "broken" and logs at ERROR so transient DB faults stay observable — but
// once the request context reaches FactExistsAt, an ordinary client disconnect
// mid-render becomes an error, and every abandoned page would emit ERROR spam
// for each ref it was resolving. Cancellation is expected, not exceptional, so
// it logs at DEBUG; everything else keeps its ERROR.
func TestReaderRefResolver_Exists_CancelledContextLogsDebug(t *testing.T) {
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

	exists := readerRefResolver{ctx: ctx, ri: r.Instance(), branch: "main"}.Exists(path)
	require.False(t, exists, "a cancelled lookup cannot claim the ref exists")

	out := buf.String()
	require.NotContains(t, out, `"level":"error"`,
		"cancellation is a routine client disconnect, not an error worth alerting on; got:\n"+out)
	require.True(t,
		strings.Contains(out, `"level":"debug"`) || out == "",
		"expected the cancellation to be recorded at DEBUG (or not at all); got:\n"+out)
}

// An expired deadline is the other routine abandonment: Exists logs it at
// DEBUG too, never ERROR. Unlike the cancellation test above, this requires the
// DEBUG record itself, so the lookup must really have failed with
// context.DeadlineExceeded. Sabotage: drop the DeadlineExceeded case (ERROR).
func TestReaderRefResolver_Exists_ExpiredDeadlineLogsDebug(t *testing.T) {
	r, path := seedCtxTestRepo(t)

	var buf bytes.Buffer
	origLogger, origLevel := log.Logger, zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() {
		log.Logger = origLogger
		zerolog.SetGlobalLevel(origLevel)
	})

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "precondition: the deadline has passed")

	exists := readerRefResolver{ctx: ctx, ri: r.Instance(), branch: "main"}.Exists(path)
	require.False(t, exists, "an expired lookup cannot claim the ref exists")

	out := buf.String()
	require.NotContains(t, out, `"level":"error"`, out)
	require.Contains(t, out, `"level":"debug"`, out)
	require.Contains(t, out, "FactExistsAt failed", out)
	require.Contains(t, out, "deadline exceeded", out)
}

// Any other lookup failure logs at ERROR with the anchor and the path, and the
// ref reads as absent. A HEAD lookup on a branch the store does not know fails
// in FactExistsAt's branch lookup, a real store error reached without a fault
// hook. Sabotage: log every failure at DEBUG, or drop the branch, path or
// commit field.
func TestReaderRefResolver_Exists_StoreErrorLogsError(t *testing.T) {
	r, path := seedCtxTestRepo(t)

	var buf bytes.Buffer
	origLogger, origLevel := log.Logger, zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() {
		log.Logger = origLogger
		zerolog.SetGlobalLevel(origLevel)
	})

	require.True(t, readerRefResolver{ctx: context.Background(), ri: r.Instance(), branch: "main"}.Exists(path),
		"precondition: the path exists on the seeded branch")
	require.Empty(t, buf.String(), "precondition: a successful lookup logs nothing")

	exists := readerRefResolver{ctx: context.Background(), ri: r.Instance(), branch: "no/such-branch"}.Exists(path)
	require.False(t, exists, "a failed lookup cannot claim the ref exists")

	out := buf.String()
	require.Contains(t, out, `"level":"error"`, out)
	require.NotContains(t, out, `"level":"debug"`, out)
	require.Contains(t, out, "FactExistsAt failed", out)
	require.Contains(t, out, `"branch":"no/such-branch"`, out)
	require.Contains(t, out, `"path":"`+path+`"`, out)
	require.Contains(t, out, `"commit":""`, out)
}
