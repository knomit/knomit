package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// lockProbingEmbedder tries to acquire SQLite's write lock from an INDEPENDENT
// connection each time it is asked to embed. Because the DB is opened with
// _txlock=immediate, BeginTx issues BEGIN IMMEDIATE — it succeeds only if no
// other transaction currently holds the write lock.
//
// This is the direct assertion for P0.5: if upsert runs inference inside its
// write transaction, this probe fails with SQLITE_BUSY.
type lockProbingEmbedder struct {
	stub768Embedder
	dsn        string
	calls      atomic.Int64
	lockedCall atomic.Int64 // times the probe found the write lock held
	probeErr   atomic.Value // first unexpected probe error
}

func (e *lockProbingEmbedder) EmbedDocument(title, body string) ([]float32, error) {
	e.calls.Add(1)

	probe, err := sql.Open("sqlite3", e.dsn)
	if err != nil {
		e.probeErr.Store(fmt.Errorf("probe open: %w", err))
		return e.stub768Embedder.EmbedDocument(title, body)
	}
	defer probe.Close()

	tx, err := probe.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		// Could not BEGIN IMMEDIATE — someone else holds the write lock.
		e.lockedCall.Add(1)
	} else {
		tx.Rollback()
	}
	return e.stub768Embedder.EmbedDocument(title, body)
}

// TestUpsert_EmbedsOutsideWriteTransaction is the regression test for P0.5.
//
// The DB runs with _txlock=immediate, so an open transaction holds SQLite's
// process-wide write lock. Embedding used to happen INSIDE upsert's transaction
// (after the COW check), which meant seconds of ONNX inference with every
// writer on every branch blocked behind it. The embedder must now be invoked
// before that transaction opens.
//
// The embedder itself asserts this: on each call it tries to BEGIN IMMEDIATE on
// a separate connection. That can only succeed if upsert is not holding the
// write lock at the moment it calls us.
func TestUpsert_EmbedsOutsideWriteTransaction(t *testing.T) {
	// A short busy_timeout keeps the probe from simply waiting out the lock —
	// we want it to fail fast and be counted if the lock IS held.
	path := filepath.Join(t.TempDir(), "k.db")
	svc, err := Open(path)
	require.NoError(t, err)
	defer svc.Close()

	emb := &lockProbingEmbedder{
		dsn: path + "?_journal_mode=WAL&_busy_timeout=200&_foreign_keys=1&_txlock=immediate",
	}
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "agent/test", "kb/a.md",
		"---\ntype: observation\n---\n# A\n\nbody-a", "add a", "test")
	require.NoError(t, err)

	require.Positive(t, emb.calls.Load(), "the write must have invoked the embedder")
	if err, ok := emb.probeErr.Load().(error); ok && err != nil {
		t.Fatalf("write-lock probe could not run: %v", err)
	}
	require.Zero(t, emb.lockedCall.Load(),
		"embedding ran while the SQLite write lock was held — inference must happen outside upsert's transaction")
}

// TestUpsert_CowHitStillSkipsInference guards the optimization the pre-tx probe
// exists to preserve: re-indexing content already in the facts table (same
// path+blob_hash) must not pay for inference at all. Hoisting the embed above
// the COW check naively would have cost an inference on every re-index.
func TestUpsert_CowHitStillSkipsInference(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	emb := &countingEmbedder{}
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ctx := context.Background()
	content := "---\ntype: observation\nentities: [alpha]\n---\n# A\n\nbody-a"

	_, err = svc.Facts().WriteFact(ctx, "agent/test", "kb/a.md", content, "add a", "test")
	require.NoError(t, err)
	first := emb.embedCalls.Load()
	require.Equal(t, int64(1), first, "first write embeds once")

	// Re-index the identical content onto a second branch: same (path,
	// blob_hash), so the COW-hit path applies and no inference is due.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/other", "agent/test"))
	_, err = svc.Facts().WriteFact(ctx, "agent/other", "kb/a.md", content, "re-add a", "test")
	require.NoError(t, err)

	require.Equal(t, first, emb.embedCalls.Load(),
		"re-indexing identical content must not re-run inference (COW hit)")
}
