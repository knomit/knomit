package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// blockingBatchEmbedder signals when embedding starts and blocks until released,
// so a test can probe the DB write lock while a rebuild is mid-embed.
type blockingBatchEmbedder struct {
	stub768Embedder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingBatchEmbedder) EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error) {
	e.once.Do(func() { close(e.started) })
	<-e.release
	out := make([][]float32, len(titles))
	for i := range out {
		out[i], _ = e.stub768Embedder.EmbedDocument(ctx, titles[i], bodies[i])
	}
	return out, nil
}

// TestRebuildEmbeddings_DoesNotHoldWriteLockDuringEmbed regresses the
// concurrency bug: the embeddings rebuild used to hold a BEGIN IMMEDIATE write
// transaction across the (slow) ONNX inference, so concurrent writers starved
// and hit "database is locked" for the whole rebuild. Embeddings are now
// computed outside the transaction, so the write lock is free while embedding.
func TestRebuildEmbeddings_DoesNotHoldWriteLockDuringEmbed(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	emb := &blockingBatchEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	svc.SetEmbedder(emb)

	ctx := context.Background()
	const branch = "main"
	for i := 0; i < 3; i++ {
		f := fact.NewFact("placeholder.md")
		f.Title = "Fact"
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = []string{"ai-governance"}
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, "kb/f"+string(rune('a'+i))+".md", out, "init", "")
		require.NoError(t, err)
	}

	si := svc.si
	_, err = si.rh.db.ExecContext(ctx, `DELETE FROM facts_vec`) // force a full re-embed
	require.NoError(t, err)

	rebuildDone := make(chan error, 1)
	go func() { rebuildDone <- svc.IndexManager().Rebuild(ctx, branch, nil) }()

	// Wait until the rebuild is inside EmbedDocuments (the slow phase).
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("rebuild never reached the embedding phase")
	}

	// The write lock must be FREE now: a BEGIN IMMEDIATE (the DSN uses
	// _txlock=immediate) must acquire it well within the busy_timeout, rather
	// than blocking for the entire embed. Before the fix this timed out.
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	wtx, err := si.rh.db.BeginTx(wctx, nil)
	require.NoError(t, err, "write lock must be free during embedding (embed runs outside the tx)")
	require.NoError(t, wtx.Rollback())

	close(emb.release)
	require.NoError(t, <-rebuildDone)
}

// shortBatchEmbedder returns FEWER vectors than entries, breaking the 1:1
// contract the insert loop relies on.
type shortBatchEmbedder struct{ stub768Embedder }

func (e *shortBatchEmbedder) EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error) {
	if len(titles) == 0 {
		return nil, nil
	}
	vec, _ := e.stub768Embedder.EmbedDocument(ctx, titles[0], bodies[0])
	return [][]float32{vec}, nil // always length 1, regardless of input size
}

// TestRebuildEmbeddings_RejectsBatchLengthMismatch regresses PR #82 review
// finding: the insert loop indexes entries[j] by vector position, so an
// embedder returning the wrong number of vectors would read out of bounds and
// panic. The rebuild must fail with a clean error instead.
func TestRebuildEmbeddings_RejectsBatchLengthMismatch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&shortBatchEmbedder{})

	ctx := context.Background()
	const branch = "main"
	for i := 0; i < 3; i++ {
		f := fact.NewFact("placeholder.md")
		f.Title = "Fact"
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = []string{"ai-governance"}
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, "kb/f"+string(rune('a'+i))+".md", out, "init", "")
		require.NoError(t, err)
	}

	si := svc.si
	_, err = si.rh.db.ExecContext(ctx, `DELETE FROM facts_vec`) // force a full re-embed
	require.NoError(t, err)

	err = svc.IndexManager().Rebuild(ctx, branch, nil)
	require.Error(t, err, "a batch length mismatch must be a clean error, not a panic")
	require.Contains(t, err.Error(), "vectors for")
}

// TestRebuild_SerializedWithConcurrentSyncLocked regresses the PR #82
// background-index race once and for all: the startup heal's Rebuild and the
// commit observer's Sync mutated a branch's index WITHOUT holding lockBranch,
// while the inline write path (notifyCommit) DID. So a background Rebuild —
// which clears the index watermark (setLastCommit "") then re-indexes every
// file — could interleave with a concurrent index Sync on the same branch,
// double-inserting into facts_vec / corrupting branch_facts.
//
// The invariant is now uniform: every index mutation on a branch holds
// lockBranch. Rebuild self-locks; out-of-band Sync callers use SyncLocked. This
// test pins the serialization: a SyncLocked issued while a Rebuild is parked
// mid-embed (holding the branch lock) MUST block until the Rebuild releases it.
func TestRebuild_SerializedWithConcurrentSyncLocked(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	emb := &blockingBatchEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	svc.SetEmbedder(emb)

	ctx := context.Background()
	const branch = "main"
	for i := 0; i < 3; i++ {
		f := fact.NewFact("placeholder.md")
		f.Title = "Fact"
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = []string{"ai-governance"}
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, "kb/f"+string(rune('a'+i))+".md", out, "init", "")
		require.NoError(t, err)
	}

	si := svc.si
	_, err = si.rh.db.ExecContext(ctx, `DELETE FROM facts_vec`) // force a full re-embed
	require.NoError(t, err)

	// Start a Rebuild; it parks inside EmbedDocuments while holding lockBranch.
	rebuildDone := make(chan error, 1)
	go func() { rebuildDone <- si.Rebuild(ctx, branch, nil) }()
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("rebuild never reached the embedding phase")
	}

	// A SyncLocked on the SAME branch must NOT proceed while the Rebuild holds
	// the per-branch lock — it must block until the Rebuild releases it.
	syncReturned := make(chan error, 1)
	go func() { syncReturned <- si.SyncLocked(ctx, branch) }()
	select {
	case <-syncReturned:
		close(emb.release)
		t.Fatal("SyncLocked ran while Rebuild held lockBranch — index mutations are NOT serialized")
	case <-time.After(200 * time.Millisecond):
		// Good: SyncLocked is parked on lockBranch.
	}

	// Release the embed: Rebuild completes, drops the lock, SyncLocked resumes.
	close(emb.release)
	require.NoError(t, <-rebuildDone)
	select {
	case err := <-syncReturned:
		require.NoError(t, err, "SyncLocked must succeed once the Rebuild has completed")
	case <-time.After(5 * time.Second):
		t.Fatal("SyncLocked never resumed after Rebuild released the branch lock")
	}
}
