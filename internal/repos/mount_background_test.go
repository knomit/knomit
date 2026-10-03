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
// rebuild on the next mount (schema marked stale AND facts_vec cleared), so
// its index job takes the heavy path. The database sits at RepoPath("kb"), so
// a mount of uid "kb" opens it. Returns the manager Home dir and the db path.
func seedReembedRepo(t *testing.T) (home, dbPath string) {
	t.Helper()
	home = t.TempDir()
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	dbPath = filepath.Join(reposDir, "kb.db")

	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "machine/test"))
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

// TestMount_BackgroundsHeavyIndex regresses the startup-blocking bug: mounting
// a repo whose index needs a heavy (re-embedding) rebuild must NOT block — the
// walk reaches Ready before the index job reports (so the HTTP server/UI come
// up), and the rebuild runs under the Index life, with the repo reporting
// "indexing" until it is "ready".
func TestMount_BackgroundsHeavyIndex(t *testing.T) {
	home, dbPath := seedReembedRepo(t)

	emb := &blockingEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	releaseOnce := sync.OnceFunc(func() { close(emb.release) })
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
		Embedder:    emb,
		// No origin is configured, so the sync loop is a harmless no-op.
	})
	t.Cleanup(func() { releaseOnce(); _ = m.Close() })

	// The mount must reply promptly even though indexing blocks in the embedder.
	_ = dbPath
	addDone := make(chan error, 1)
	go func() {
		ri, err := m.mountExisting("kb", "kb", nil)
		if err == nil {
			m.Set("kb", ri)
		}
		addDone <- err
	}()
	select {
	case err := <-addDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the mount blocked on indexing — the index job is not in the background")
	}

	// Background heal must be running (reached the embedder) and report indexing.
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background index never reached the embedding phase")
	}
	ri := m.Get("kb")
	require.NotNil(t, ri)
	st := ri.Status()
	require.Equal(t, "ready", st.Stage, "the walk reached Ready before the index job reported")
	require.Equal(t, IndexStateIndexing, st.Index.State, "repo must report 'indexing' while the rebuild runs")

	// Unblock; it must reach ready.
	releaseOnce()
	require.Equal(t, IndexStateReady, waitIndexSettled(t, ri).Index.State,
		"repo must reach 'ready' after the rebuild completes")
}

// TestManagerClose_WaitsForBackgroundIndex regresses PR #82 review finding #1:
// a teardown that closed the store while the index job was still issuing SQL
// on the same *sql.DB — a use-after-close. Unmount drains the Index life
// before the Open stage closes the store, so Close must block until the
// in-flight job returns (this embedder ignores its ctx, the worst case).
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

	_ = dbPath
	ri, err := m.mountExisting("kb", "kb", nil)
	require.NoError(t, err)
	m.Set("kb", ri)

	// The job is now parked inside EmbedDocuments (write tx not yet opened).
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background index never reached the embedding phase")
	}

	// Close must NOT complete while the job is still in flight: it cancels the
	// machine's root ctx (which the blocked embed ignores) and drains.
	closeDone := make(chan struct{})
	go func() { _ = m.Close(); close(closeDone) }()
	select {
	case <-closeDone:
		t.Fatal("Manager.Close returned while the index job was still running — it closed the store out from under it")
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

// TestMount_FailedBackgroundIndexReportsError regresses PR #82 review finding
// #1: an index job that genuinely FAILS must report "error" — with a reason —
// not falsely report "ready".
func TestMount_FailedBackgroundIndexReportsError(t *testing.T) {
	home, dbPath := seedReembedRepo(t)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
		Embedder:    failingEmbedder{},
	})
	t.Cleanup(func() { _ = m.Close() })

	_ = dbPath
	ri, err := m.mountExisting("kb", "kb", nil)
	require.NoError(t, err)
	m.Set("kb", ri)

	st := waitIndexSettled(t, ri)
	require.Equal(t, IndexStateError, st.Index.State, "a failed rebuild must report 'error', not 'ready'")
	require.NotEmpty(t, st.Index.Reason, "and say why")
}

// EmbedShortStrings satisfies store.BatchEmbedder. Short strings render
// through the model's short-string template in production; a stub has no
// template, so it embeds each string as a title-only document.
func (e *blockingEmbedder) EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error) {
	return e.EmbedDocuments(ctx, texts, make([]string, len(texts)))
}

// EmbedShortStrings satisfies store.BatchEmbedder. Short strings render
// through the model's short-string template in production; a stub has no
// template, so it embeds each string as a title-only document.
func (e testEmbedder) EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error) {
	return e.EmbedDocuments(ctx, texts, make([]string, len(texts)))
}

// EmbedShortStrings satisfies store.BatchEmbedder for the failing stub too.
func (e failingEmbedder) EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error) {
	return e.EmbedDocuments(ctx, texts, make([]string, len(texts)))
}
