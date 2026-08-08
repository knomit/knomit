package repos

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/store"
)

type testEmbedder struct{}

func (testEmbedder) Thresholds() params.Thresholds { return params.Defaults() }
func (testEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	v := make([]float32, 768)
	v[0] = 1
	return v, nil
}
func (testEmbedder) EmbedDocument(context.Context, string, string) ([]float32, error) {
	v := make([]float32, 768)
	v[0] = 1
	return v, nil
}
func (testEmbedder) Dim() int   { return 768 }
func (testEmbedder) ID() string { return "test768" }
func (testEmbedder) EmbedDocuments(_ context.Context, titles, _ []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range out {
		out[i] = make([]float32, 768)
		out[i][0] = 1
	}
	return out, nil
}

type blockingEmbedder struct {
	testEmbedder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingEmbedder) EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error) {
	e.once.Do(func() { close(e.started) })
	<-e.release
	return e.testEmbedder.EmbedDocuments(ctx, titles, bodies)
}

// seedReembedRepo creates a repo with a few facts and forces a re-embedding
// rebuild on the next open (schema marked stale AND facts_vec cleared), so
// opening it takes the heavy background-index path. Returns the manager Home
// dir and the repo db path.
func seedReembedRepo(t *testing.T) (home, dbPath string) {
	t.Helper()
	home = t.TempDir()
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	dbPath = filepath.Join(reposDir, "kb.db")

	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{}, "machine/test"))
	svc.SetEmbedder(testEmbedder{})
	for i := 0; i < 3; i++ {
		f := fact.NewFact("placeholder.md")
		f.Title = "F"
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = []string{"ai-governance"}
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(context.Background(), "machine/test", "kb/f"+string(rune('a'+i))+".md", out, "init", "")
		require.NoError(t, werr)
	}
	require.NoError(t, svc.Close())

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	// Force the heavy path on the next open: drop every branch's index schema
	// version so each reports stale (→ full Rebuild), and empty facts_vec so that
	// rebuild has to re-embed rather than reuse vectors.
	_, err = raw.Exec(`DELETE FROM meta WHERE key GLOB 'graph_schema_version:*'`)
	require.NoError(t, err)
	_, err = raw.Exec(`DELETE FROM facts_vec`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	return home, dbPath
}

// TestOpenOne_BackgroundsHeavyIndex regresses the startup-blocking bug: opening
// a repo whose index needs a heavy (re-embedding) rebuild must NOT block — the
// store comes up immediately (so the HTTP server/UI do too) and the rebuild
// runs in the background, with the repo reporting "indexing" until it's "ready".
func TestOpenOne_BackgroundsHeavyIndex(t *testing.T) {
	home, dbPath := seedReembedRepo(t)

	emb := &blockingEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	releaseOnce := sync.OnceFunc(func() { close(emb.release) })
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
		Embedder:    emb,
		// NOTE: DisableBackgroundSync is intentionally false so openOne takes
		// the production BACKGROUND path (with it set, open indexes inline).
		// No origin is configured, so the sync loops are harmless no-ops.
	})
	t.Cleanup(func() { releaseOnce(); _ = m.Close() })

	// Add must return promptly even though indexing blocks in the embedder.
	addDone := make(chan error, 1)
	go func() { addDone <- m.Add("kb", dbPath) }()
	select {
	case err := <-addDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Add blocked on indexing — open did not background the heavy rebuild")
	}

	// Background heal must be running (reached the embedder) and report indexing.
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background index never reached the embedding phase")
	}
	ri := m.Get("kb")
	require.NotNil(t, ri)
	state, _, _ := ri.IndexStatus()
	require.Equal(t, "indexing", state, "repo must report 'indexing' while the background rebuild runs")

	// Unblock; it must reach ready.
	releaseOnce()
	require.Eventually(t, func() bool {
		s, _, _ := ri.IndexStatus()
		return s == "ready"
	}, 10*time.Second, 50*time.Millisecond, "repo must reach 'ready' after the background rebuild completes")
}

// TestManagerClose_WaitsForBackgroundIndex regresses PR #82 review finding #1:
// the background heal goroutine was not tracked by ri.syncWg, so Manager.Close
// (and Archive/SwapStore) ran svc.Close() while the heal was still issuing SQL
// on the same *sql.DB — a use-after-close. Close must now block until the
// in-flight heal returns.
func TestManagerClose_WaitsForBackgroundIndex(t *testing.T) {
	home, dbPath := seedReembedRepo(t)

	emb := &blockingEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	releaseOnce := sync.OnceFunc(func() { close(emb.release) })
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
		Embedder:    emb,
	})
	t.Cleanup(func() { releaseOnce() })

	require.NoError(t, m.Add("kb", dbPath))

	// Heal is now parked inside EmbedDocuments (write tx not yet opened).
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background index never reached the embedding phase")
	}

	// Close must NOT complete while the heal is still in-flight: it cancels the
	// sync ctx (which the blocked embed ignores) and then waits on syncWg.
	closeDone := make(chan struct{})
	go func() { _ = m.Close(); close(closeDone) }()
	select {
	case <-closeDone:
		t.Fatal("Manager.Close returned while the background index was still running — it closed the store out from under the heal")
	case <-time.After(300 * time.Millisecond):
		// Good: Close is blocked waiting for the heal.
	}

	// Releasing the embed lets the heal finish; Close must then return.
	releaseOnce()
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Manager.Close did not return after the background index completed")
	}
}

// failingEmbedder fails every batch embed, forcing the background rebuild to
// return an error.
type failingEmbedder struct{ testEmbedder }

func (failingEmbedder) EmbedDocuments(context.Context, []string, []string) ([][]float32, error) {
	return nil, errors.New("embed boom")
}

// TestOpenOne_FailedBackgroundIndexReportsError regresses PR #82 review finding
// #1: a background index that genuinely FAILS must report "error", not falsely
// report "ready". Before the fix healIndexBranches swallowed all errors and the
// caller unconditionally marked the repo ready; "error" was only ever set on a
// clean shutdown (a non-failure).
func TestOpenOne_FailedBackgroundIndexReportsError(t *testing.T) {
	home, dbPath := seedReembedRepo(t)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
		Embedder:    failingEmbedder{},
		// Background path (DisableBackgroundSync false); no origin configured.
	})
	t.Cleanup(func() { _ = m.Close() })

	require.NoError(t, m.Add("kb", dbPath))
	ri := m.Get("kb")
	require.NotNil(t, ri)

	require.Eventually(t, func() bool {
		s, _, _ := ri.IndexStatus()
		return s == "error"
	}, 10*time.Second, 50*time.Millisecond, "a failed background rebuild must report 'error', not 'ready'")
}
