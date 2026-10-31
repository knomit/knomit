package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/merge"
)

// writeFactOn writes a fact with refs on branch (writeExplainFact always
// writes on explainTestBranch).
func writeFactOn(t *testing.T, ctx context.Context, ri *repos.RepoInstance, branch, path, title string, refs []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = title + " body text."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Entities = []string{}
	f.Refs = refs
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteFact(ctx, branch, path, content, "write "+path, "")
		require.NoError(t, werr)
	})
}

// "Changed since cited" is a BLOB comparison (PR #693 round 5, F1; user
// ruling 2026-10-30): R and X (citing R) are written on a side branch, the
// reading branch moves on, and the side branch is merged in with both
// unchanged. explain on the reading branch must NOT flag R superseded: the
// branch's latest commit_log row for R is now the merge, but its blob is the
// one X cited. Once the reading branch really changes R, it is superseded.
//
// Sabotage: compare the branch's latest commit for R with the pin again (the
// pre-fix readNode), and R is flagged superseded right after the merge.
func TestExplain_SupersededIsABlobComparisonAfterAMerge(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	writeExplainFact(t, ctx, ri, "kb/base.md", "Base", 0.5, nil)
	ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(ctx, "side", explainTestBranch))
	})
	writeFactOn(t, ctx, ri, "side", "kb/r.md", "R", nil)
	writeFactOn(t, ctx, ri, "side", "kb/x.md", "X", []string{"kb/r.md"})
	writeExplainFact(t, ctx, ri, "kb/z.md", "Z", 0.5, nil)
	ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Merge().MergeBranch(ctx, "side", explainTestBranch, merge.StrategyLocalWins))
	})

	r := findExpFact(explainAll(t, ctx, "kb/x.md", ""), "kb/r.md")
	require.NotNil(t, r, "X's ref to R is walked on the merged-into branch")
	require.False(t, r.Deleted)
	require.False(t, r.Superseded, "R arrived unchanged by the merge: not superseded")

	writeExplainFact(t, ctx, ri, "kb/r.md", "R changed", 0.5, nil)
	r = findExpFact(explainAll(t, ctx, "kb/x.md", ""), "kb/r.md")
	require.NotNil(t, r)
	require.Equal(t, "R", r.Title, "the pinned version")
	require.True(t, r.Superseded, "the branch's R blob now differs from the cited one")
	require.False(t, r.Deleted)
}
