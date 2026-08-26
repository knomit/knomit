package synthesize

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestAcceptance_RealCorpusShortlist runs the consolidation-scope fix against a
// REAL knomit repo database with the REAL embedding model, and prints what the
// shortlist did with it.
//
// Skipped unless KNOMIT_PHASE0_DB names a copy of a repo DB, because it needs
// both a corpus and the ONNX model, and because it is a measurement rather than
// a contract — the assertions below are deliberately weak (it must not crash,
// and it must produce a bounded batch). What makes it worth keeping is the
// output: coverage, the standing pair population, this corpus's operating
// point, and the pairs it chose to spend judge slots on.
//
//	cp ~/.knomit/repos/<uid>.db /tmp/accept.db
//	KNOMIT_PHASE0_DB=/tmp/accept.db go test ./internal/synthesize/ \
//	    -run TestAcceptance_RealCorpusShortlist -v -timeout 30m
//
// Work on a COPY. The run migrates the schema and writes derived state.
func TestAcceptance_RealCorpusShortlist(t *testing.T) {
	dbPath := os.Getenv("KNOMIT_PHASE0_DB")
	if dbPath == "" {
		t.Skip("set KNOMIT_PHASE0_DB to a COPY of a repo database to run the acceptance measurement")
	}
	ctx := context.Background()

	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	model, err := embeddings.Lookup(embeddings.DefaultModelID)
	require.NoError(t, err)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	emb, err := embeddings.NewEmbedder(ctx, model, filepath.Join(home, ".knomit", "models"))
	require.NoError(t, err)
	t.Cleanup(emb.Close)
	svc.SetEmbedder(emb)
	require.NoError(t, svc.OpenRepo())

	branch := os.Getenv("KNOMIT_PHASE0_BRANCH")
	if branch == "" {
		branch, err = svc.Branches().DefaultBranch(ctx)
		require.NoError(t, err)
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "acceptance",
		AgentBranch:  branch,
		Svc:          svc,
		Embedder:     emb,
		OntologyRoot: "kb",
	})
	d := Deps{
		RI:          ri,
		Facts:       svc.Facts(),
		Search:      svc.Search(),
		Pipeline:    svc.Pipeline(),
		Branches:    svc.Branches(),
		Abstraction: svc.Abstraction(),
		Effort:      EffortNormal,
		OnProgress:  func(ProgressEvent) {},
	}

	// Fill the axis. In production this spreads over sessions under the latency
	// budget; here we keep going so the measurement sees full coverage, and
	// report what a first session would actually have cost.
	firstSession := time.Now()
	have, total, err := ensureTitleVectors(ctx, d, branch, titleBackfillBudget)
	require.NoError(t, err)
	t.Logf("first session backfill: %d/%d titles in %s", have, total, time.Since(firstSession).Round(time.Millisecond))

	for have < total {
		before := have
		have, total, err = ensureTitleVectors(ctx, d, branch, titleBackfillBudget)
		require.NoError(t, err)
		require.Greater(t, have, before, "backfill stalled at %d/%d", have, total)
	}
	t.Logf("coverage complete: %d/%d", have, total)

	seeded := time.Now()
	refresh, err := refreshRestatementShortlist(ctx, d, branch)
	require.NoError(t, err)
	t.Logf("shortlist seeded in %s (%d KNN queries, %d pairs, %d partners requeued)",
		time.Since(seeded).Round(time.Millisecond),
		refresh.NeighbourQueries, refresh.PairsAdded, refresh.FactsRequeued)

	pairs, health, err := selectRestatementCandidates(ctx, d, branch, nil, total)
	require.NoError(t, err)

	for _, line := range healthLines(health) {
		t.Log(line)
	}
	require.LessOrEqual(t, len(pairs), maxShortlistItems,
		"whatever the corpus looks like, a session's spend is bounded")

	for i, p := range pairs {
		t.Logf("candidate %d (title-cos %.3f)", i, p.TitleCos)
		t.Logf("  A %s", p.APath)
		t.Logf("  B %s", p.BPath)
	}

	// The whole standing population, for corpora small enough to read.
	standing, err := svc.Abstraction().RestatementPairsByRank(ctx, branch, 25)
	require.NoError(t, err)
	t.Logf("top standing pairs (%d shown of %d):", len(standing), health.StandingPairs)
	for _, p := range standing {
		t.Logf("  %.3f  %s | %s", p.TitleCos, p.APath, p.BPath)
	}
}
