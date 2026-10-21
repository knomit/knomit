package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// #533 review B2, the reviewer's probe made a test: connecting a repository
// that holds a LEGACY invalid fact to a remote with disjoint history
// (Replay, the origin session) copies every local fact into the clone of the
// remote. A copy is not an edit: the legacy fact is carried over unjudged
// (WithCarriedOver) and the connect completes with every fact; an ordinary
// edit of the same fact in the clone afterwards is still judged.
//
// SABOTAGE: drop WithCarriedOver from Replay's write -> Replay fails on the
// legacy fact and the clone holds only what came before it -> red; ignore
// the flag in writeFileExact -> red.
func TestValidateWrite_ACopyOfALegacyInvalidFactIsCarriedOver(t *testing.T) {
	ctx := context.Background()
	const agent = "agent/laptop"
	ont := map[string]string{fact.OntologyFile: validatedOntology("")}
	local, err := Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	require.NoError(t, local.InitRepo(ctx, ont, agent))
	_, err = local.Facts().WriteFact(ctx, agent, "kb/notes/a.md", note("a", "e"), "a", "")
	require.NoError(t, err)
	_, err = local.rh.CommitFilesForTest(agent, map[string]string{"kb/notes/legacy.md": note("legacy")}, "legacy, before the rule")
	require.NoError(t, err)
	_, err = local.Facts().WriteFact(ctx, agent, "kb/notes/z.md", note("z", "e"), "z", "")
	require.NoError(t, err)

	clone, err := Open(filepath.Join(t.TempDir(), "clone.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = clone.Close() })
	require.NoError(t, clone.InitRepo(ctx, ont, "main"))

	iter, err := local.Search().FactsIter(ctx, agent)
	require.NoError(t, err)
	res, err := Replay(ctx, local, agent, iter, clone, ReplayConfig{Strategy: StrategyLocalWins, AgentBranch: agent, DefaultBranch: "main"})
	require.NoError(t, err, "connecting a repository with a legacy invalid fact")
	require.Equal(t, 3, res.FromLocal)
	paths, err := clone.rh.ListAll(ctx, agent)
	require.NoError(t, err)
	require.Subset(t, paths, []string{"kb/notes/a.md", "kb/notes/legacy.md", "kb/notes/z.md"})

	// An ordinary write is still judged: an edit of the carried-over fact
	// that does not fix it is refused.
	_, err = clone.Facts().WriteFact(ctx, agent, "kb/notes/legacy.md", note("legacy, edited"), "edit", "")
	require.ErrorIs(t, err, ErrFactInvalid)
}

// #533 re-review N2: a legacy invalid fact whose content Replay ALTERS (a
// dead ref resolveDeadRefs drops) is new content, not a copy: it is not
// marked carried over and is judged, so the connect is refused naming it
// (the choice: altered content is validated, not waved through).
//
// SABOTAGE: mark every Replay write carried over (altered content
// included) -> the altered invalid fact is written unjudged -> red.
func TestValidateWrite_AReplayThatAltersALegacyInvalidFactJudgesIt(t *testing.T) {
	ctx := context.Background()
	const agent = "agent/laptop"
	ont := map[string]string{fact.OntologyFile: validatedOntology("")}
	local, err := Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	require.NoError(t, local.InitRepo(ctx, ont, agent))
	f := fact.NewFact("placeholder.md")
	f.Title, f.Confidence, f.Sources, f.Type = "legacy", 0.9, 1, fact.Observation
	f.Refs = []string{"kb/notes/gone.md"} // dead: neither side holds it
	legacy, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = local.rh.CommitFilesForTest(agent, map[string]string{"kb/notes/legacy.md": legacy}, "legacy, before the rule")
	require.NoError(t, err)
	// An ordinary write after it syncs the index (CommitFilesForTest does not).
	_, err = local.Facts().WriteFact(ctx, agent, "kb/notes/z.md", note("z", "e"), "z", "")
	require.NoError(t, err)

	clone, err := Open(filepath.Join(t.TempDir(), "clone.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = clone.Close() })
	require.NoError(t, clone.InitRepo(ctx, ont, "main"))
	iter, err := local.Search().FactsIter(ctx, agent)
	require.NoError(t, err)
	_, err = Replay(ctx, local, agent, iter, clone, ReplayConfig{Strategy: StrategyLocalWins, AgentBranch: agent, DefaultBranch: "main"})
	require.ErrorIs(t, err, ErrFactInvalid, "the altered fact is judged")
	require.Contains(t, err.Error(), "kb/notes/legacy.md")
}
