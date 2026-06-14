package repos

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/retrieval"
	"knomit/internal/store"
)

type testEmbedder struct{}

func (testEmbedder) Thresholds() retrieval.Thresholds            { return retrieval.Defaults() }
func (testEmbedder) EmbedQuery(string) ([]float32, error)         { v := make([]float32, 768); v[0] = 1; return v, nil }
func (testEmbedder) EmbedDocument(_, _ string) ([]float32, error) { v := make([]float32, 768); v[0] = 1; return v, nil }
func (testEmbedder) Dim() int                                     { return 768 }
func (testEmbedder) ID() string                                   { return "test768" }
func (testEmbedder) EmbedDocuments(titles, _ []string) ([][]float32, error) {
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

func (e *blockingEmbedder) EmbedDocuments(titles, bodies []string) ([][]float32, error) {
	e.once.Do(func() { close(e.started) })
	<-e.release
	return e.testEmbedder.EmbedDocuments(titles, bodies)
}

// TestOpenOne_BackgroundsHeavyIndex regresses the startup-blocking bug: opening
// a repo whose index needs a heavy (re-embedding) rebuild must NOT block — the
// store comes up immediately (so the HTTP server/UI do too) and the rebuild
// runs in the background, with the repo reporting "indexing" until it's "ready".
func TestOpenOne_BackgroundsHeavyIndex(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	dbPath := filepath.Join(reposDir, "kb.db")

	// Seed a repo with facts, then force a re-embedding rebuild on next open:
	// mark the schema stale AND clear facts_vec so the rebuild must re-embed.
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
	require.NoError(t, svc.IndexManager().MarkRebuildNeeded(context.Background()))
	require.NoError(t, svc.Close())

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec(`DELETE FROM facts_vec`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	emb := &blockingEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	releaseOnce := sync.OnceFunc(func() { close(emb.release) })
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		Embedder:              emb,
		DisableBackgroundSync: true,
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
