package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// THE TEMPORAL GRAPH, verified against the ref-classification rewrite.
//
// kb/principles/philosophy/historical-not-current: every ref, edge and
// provenance link resolves at the commit of its REFERRER, never at HEAD.
// This PR replaced the rule that decides which refs become edges
// (localFactRefs → fact.ClassifyRef) and changed the stored form of a local
// ref (bare → kb://<own-id>/<path>). Both sit directly under the edge builder,
// so these assert the temporal behaviour survives both.

func newTemporalRepo(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc, "main"
}

// A DERIVED_FROM edge binds (source@sourceCommit → target@targetCommit). Once
// the target is retracted, the edge must still be there and still point at the
// target's last valid version: the source said "I derive from that" and that
// statement is about the past. If the edge vanished, "why is this true?" would
// become unanswerable the moment anything it rested on was retracted.
func TestTemporal_EdgeSurvivesTargetRetraction(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTemporalRepo(t)

	tgt, err := svc.Facts().WriteFact(ctx, branch, "kb/target.md", testFactBody("target", 0.9, nil), "init target", "")
	require.NoError(t, err)
	src, err := svc.Facts().WriteFact(ctx, branch, "kb/source.md",
		testFactBody("source", 0.8, []string{"kb/target.md"}), "source→target", "")
	require.NoError(t, err)

	before, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/source.md", src.CommitHash)
	require.NoError(t, err)
	require.Len(t, before, 1, "precondition: the edge must exist while the target is live")
	require.Equal(t, tgt.CommitHash, before[0].Commit,
		"the edge must pin the target's version, not just its path")

	// Retract the target, then advance the branch past it.
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/target.md", "retract target")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/unrelated.md", testFactBody("u", 0.5, nil), "advance", "")
	require.NoError(t, err)

	after, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/source.md", src.CommitHash)
	require.NoError(t, err)
	require.Len(t, after, 1, "the edge must survive retraction of its target")
	require.Equal(t, "kb/target.md", after[0].Path)
	require.Equal(t, tgt.CommitHash, after[0].Commit,
		"the edge must still point at the version the source reasoned over")
}

// The canonical stored form must build the SAME edge as the bare form. This
// PR made kb://<own-id>/<path> the form every local ref is written in, so if
// the edge builder failed to recognise it, every fact written after the change
// would silently have no lineage at all.
func TestTemporal_CanonicalRefFormBuildsTheSameEdge(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTemporalRepo(t)

	root, err := svc.RootCommit(ctx, branch)
	require.NoError(t, err)
	id := fact.ID12(root)

	tgt, err := svc.Facts().WriteFact(ctx, branch, "kb/target.md", testFactBody("target", 0.9, nil), "init target", "")
	require.NoError(t, err)

	bare, err := svc.Facts().WriteFact(ctx, branch, "kb/bare.md",
		testFactBody("bare", 0.8, []string{"kb/target.md"}), "bare→target", "")
	require.NoError(t, err)
	canon, err := svc.Facts().WriteFact(ctx, branch, "kb/canon.md",
		testFactBody("canon", 0.8, []string{fact.QualifyKBPath(id, "kb/target.md")}), "canon→target", "")
	require.NoError(t, err)

	bareEdges, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/bare.md", bare.CommitHash)
	require.NoError(t, err)
	canonEdges, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/canon.md", canon.CommitHash)
	require.NoError(t, err)

	require.Len(t, bareEdges, 1, "control: the bare form must build an edge")
	require.Len(t, canonEdges, 1,
		"the canonical stored form must build an edge too — otherwise every fact written "+
			"after canonicalization landed silently has no lineage")
	require.Equal(t, bareEdges[0].Path, canonEdges[0].Path)
	require.Equal(t, tgt.CommitHash, canonEdges[0].Commit,
		"the canonical form must pin the same target version as the bare form")
}

// A ref to a fact that was ALREADY retracted when the referrer was written
// still forms an edge, pinned to the target's last valid version. This is the
// supersession citation — "this replaces the thing we retracted" — and it is
// the read-side counterpart of what the write gate now accepts.
func TestTemporal_EdgeToAlreadyRetractedTargetResolvesToLastValidVersion(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTemporalRepo(t)

	tgt, err := svc.Facts().WriteFact(ctx, branch, "kb/target.md", testFactBody("target", 0.9, nil), "init target", "")
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/target.md", "retract target")
	require.NoError(t, err)

	src, err := svc.Facts().WriteFact(ctx, branch, "kb/successor.md",
		testFactBody("successor", 0.8, []string{"kb/target.md"}), "successor→retracted target", "")
	require.NoError(t, err)

	edges, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/successor.md", src.CommitHash)
	require.NoError(t, err)
	require.Len(t, edges, 1,
		"citing an already-retracted fact must still form an edge — the target has a navigable last-valid blob")
	require.Equal(t, tgt.CommitHash, edges[0].Commit,
		"the edge must resolve to the target's last valid version, not to nothing")
}

// Rebuilding the index from scratch must reproduce the same versioned edges.
// The rebuild path re-classifies every HISTORICAL fact version with today's
// rule, so a rule that read history differently than the incremental path did
// would silently rewrite the graph on the next rebuild.
func TestTemporal_RebuildReproducesHistoricalEdges(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTemporalRepo(t)

	root, err := svc.RootCommit(ctx, branch)
	require.NoError(t, err)
	id := fact.ID12(root)

	tgt, err := svc.Facts().WriteFact(ctx, branch, "kb/target.md", testFactBody("target", 0.9, nil), "init target", "")
	require.NoError(t, err)
	// One of each stored form, so the rebuild is exercised on both.
	bare, err := svc.Facts().WriteFact(ctx, branch, "kb/bare.md",
		testFactBody("bare", 0.8, []string{"kb/target.md"}), "bare→target", "")
	require.NoError(t, err)
	canon, err := svc.Facts().WriteFact(ctx, branch, "kb/canon.md",
		testFactBody("canon", 0.8, []string{fact.QualifyKBPath(id, "kb/target.md")}), "canon→target", "")
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/target.md", "retract target")
	require.NoError(t, err)

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	for _, tc := range []struct {
		path, commit string
	}{
		{"kb/bare.md", bare.CommitHash},
		{"kb/canon.md", canon.CommitHash},
	} {
		edges, err := svc.Search().OutgoingAtCommit(ctx, branch, tc.path, tc.commit)
		require.NoError(t, err)
		require.Lenf(t, edges, 1, "%s: rebuild must reproduce the historical edge", tc.path)
		require.Equalf(t, tgt.CommitHash, edges[0].Commit,
			"%s: rebuild must pin the same target version", tc.path)
	}
}
