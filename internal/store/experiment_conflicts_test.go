package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// An experiment commit honors the repo's `conflicts` setting, read at the tip
// of the CONSENSUS branch ("main" in newExperimentTestStore) exactly where
// every other merge site reads it. The parent (the agent branch) is the
// consensus side; whatever a key set to off leaves is refused as before.

const expF = "kb/notes/f.md"

// expConflictFork forks experiment "x" from the agent branch after F exists,
// sets `ontology` (cmOntology shape) on the CONSENSUS branch when non-empty,
// then has both sides rewrite F: the experiment more confident than the
// parent, and the parent adding an entity.
func expConflictFork(t *testing.T, ontology string) (*Service, *cmRepo) {
	t.Helper()
	svc := newExperimentTestStore(t)
	r := &cmRepo{t: t, svc: svc}
	r.write(testAgentBranch, expF, cmFact(t, "base", 0.7))
	if ontology != "" {
		r.write("main", fact.OntologyFile, ontology)
	}
	_, err := svc.Experiments().OpenExperiment(context.Background(), "x", "", testAgentBranch)
	require.NoError(t, err)
	r.write("exp/x", expF, cmFact(t, "experiment body", 0.9))
	r.write(testAgentBranch, expF, cmFact(t, "parent body", 0.6, "ParentAdded"))
	return svc, r
}

func expStillOpen(t *testing.T, svc *Service) bool {
	t.Helper()
	_, ok, err := svc.Experiments().GetExperiment(context.Background(), "x")
	require.NoError(t, err)
	return ok
}

// (a) `facts: merge`: the commit is no longer refused. The fact is
// field-merged (confidence rule: the more confident experiment's body, the
// parent's added entity), the merge commit carries the Knomit-Merge line, the
// result reports the path as merged, and the experiment is gone.
//
// SABOTAGE S1a (CommitExperiment ignores the setting) → refused → red.
func TestCommitExperiment_ConflictsMerge_FieldMerges(t *testing.T) {
	svc, r := expConflictFork(t, cmOntology("merge"))
	res, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
	require.NoError(t, err, "the setting settles the conflict; no refusal")
	require.Equal(t, ModeMerge, res.Mode)

	tip := r.tip(testAgentBranch)
	_, out := r.blob(tip, expF)
	f := r.parse(out)
	require.Equal(t, "experiment body", f.Body)
	require.Equal(t, 0.9, f.Confidence)
	require.Equal(t, []string{"ParentAdded"}, f.Entities)
	require.Len(t, TrailerValues(tip.Message, TrailerMerge), 1)
	require.Contains(t, tip.Message, "(conflicts:merge/off)")
	require.Equal(t, []SettledPath{{Path: expF, Kept: "merged"}}, res.Settled)
	require.False(t, expStillOpen(t, svc), "a successful commit deletes the experiment")
}

// (b) No setting: refused exactly as before, nothing moves.
func TestCommitExperiment_NoConflictsSetting_StillRefuses(t *testing.T) {
	svc, r := expConflictFork(t, "")
	before := r.tip(testAgentBranch).Hash
	_, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{expF}, conflict.Paths)
	require.Equal(t, before, r.tip(testAgentBranch).Hash)
	require.True(t, expStillOpen(t, svc))
}

// (d) `facts: merge:consensus`: the fields both sides changed take the
// PARENT's values (the consensus side is dst), even against a more confident
// experiment.
//
// SABOTAGE S1b (consensus side = src) → the experiment's body → red.
func TestCommitExperiment_MergeConsensus_ParentIsConsensusSide(t *testing.T) {
	svc, r := expConflictFork(t, cmOntology("merge:consensus"))
	// A field only the experiment changed (the title), so the merge is not
	// simply the parent's version.
	r.write("exp/x", expF, strings.Replace(cmFact(t, "experiment body", 0.9), "# Shared fact", "# Experiment title", 1))
	res, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode)
	_, out := r.blob(r.tip(testAgentBranch), expF)
	f := r.parse(out)
	require.Equal(t, "Experiment title", f.Title, "a field only the experiment changed is kept")
	require.Equal(t, "parent body", f.Body)
	require.Equal(t, 0.6, f.Confidence)
	require.Contains(t, TrailerValues(r.tip(testAgentBranch).Message, TrailerMerge)[0], "strategy=merge_consensus ")
}

// (R2) `facts: consensus`: the parent's whole version lands and the
// experiment's edit is DROPPED — and the result says so, per path.
func TestCommitExperiment_FactsConsensus_ReportsTheDrop(t *testing.T) {
	svc, r := expConflictFork(t, cmOntology("consensus"))
	parentF := cmFact(t, "parent body", 0.6, "ParentAdded")
	res, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
	require.NoError(t, err)
	_, out := r.blob(r.tip(testAgentBranch), expF)
	require.Equal(t, parentF, out)
	require.Equal(t, []SettledPath{{Path: expF, Kept: "dst", Dropped: "src-modify"}}, res.Settled)
}

// Deletion wins over an edit under `facts: merge`: the parent retracted F,
// the experiment edited it. F is absent after the commit, and the result says
// the kept side's version is a deletion.
func TestCommitExperiment_ConflictsMerge_ParentDeletionWins(t *testing.T) {
	svc := newExperimentTestStore(t)
	r := &cmRepo{t: t, svc: svc}
	r.write(testAgentBranch, expF, cmFact(t, "base", 0.7))
	r.write("main", fact.OntologyFile, cmOntology("merge"))
	_, err := svc.Experiments().OpenExperiment(context.Background(), "x", "", testAgentBranch)
	require.NoError(t, err)
	r.write("exp/x", expF, cmFact(t, "experiment body", 0.9))
	r.del(testAgentBranch, expF)

	res, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
	require.NoError(t, err)
	h, _ := r.blob(r.tip(testAgentBranch), expF)
	require.True(t, h.IsZero(), "the retraction wins")
	require.Equal(t, []SettledPath{{Path: expF, Kept: "dst", Dropped: "src-modify", Deleted: true}}, res.Settled)
}

// (c)+(j) `facts: merge, state: off`, one fact conflict and one non-fact
// conflict. The first commit is refused naming ONLY the non-fact path (the
// setting settled the fact). The retry that resolves exactly that path, as
// the refusal said to, succeeds: the setting still field-merges the fact and
// the caller's choice lands for the non-fact, recorded as chosen.
//
// SABOTAGE S1c (fallback LocalWins, not Refuse) → the first commit succeeds →
// red. SABOTAGE S1d (resolutions present ⇒ setting off) → the retry is refused
// for the fact → red.
func TestCommitExperiment_ConflictsMerge_RetryKeepsTheSetting(t *testing.T) {
	svc, r := expConflictFork(t, cmOntology("merge/off"))
	r.write("exp/x", "notes.txt", "experiment\n")
	r.write(testAgentBranch, "notes.txt", "parent\n")
	before := r.tip(testAgentBranch).Hash

	_, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"notes.txt"}, conflict.Paths, "the setting settled the fact; only the non-fact is left")
	require.Equal(t, before, r.tip(testAgentBranch).Hash, "a refusal moves nothing")
	require.True(t, expStillOpen(t, svc))

	res, err := svc.Experiments().CommitExperiment(context.Background(), "x",
		map[string]Resolution{"notes.txt": {Side: ResolveSrc}})
	require.NoError(t, err)
	tip := r.tip(testAgentBranch)
	_, notes := r.blob(tip, "notes.txt")
	require.Equal(t, "experiment\n", notes)
	_, out := r.blob(tip, expF)
	require.Equal(t, "experiment body", r.parse(out).Body, "the fact is still field-merged by the setting")
	require.Len(t, TrailerValues(tip.Message, TrailerMerge), 1)
	conflicts := TrailerValues(tip.Message, TrailerConflict)
	require.Len(t, conflicts, 1)
	require.Regexp(t, `^notes\.txt kept=src dropped=dst-add strategy=refuse base=none .* reason=chosen$`, conflicts[0])
	require.Equal(t, []SettledPath{
		{Path: expF, Kept: "merged"},
		{Path: "notes.txt", Kept: "src", Dropped: "dst-add", Chosen: true},
	}, res.Settled)
}

// (e) A caller resolution on a fact path the setting would have merged wins
// over the setting: "theirs" keeps the parent's bytes exactly, and the
// setting's Knomit-Merge line for it is replaced by the chosen record.
//
// SABOTAGE S1f (overlay applied but the setting's line kept) → a Knomit-Merge
// line remains → red.
func TestCommitExperiment_ConflictsMerge_CallerResolutionWins(t *testing.T) {
	svc, r := expConflictFork(t, cmOntology("merge"))
	parentF := cmFact(t, "parent body", 0.6, "ParentAdded")
	res, err := svc.Experiments().CommitExperiment(context.Background(), "x",
		map[string]Resolution{expF: {Side: ResolveDst}})
	require.NoError(t, err)
	_, out := r.blob(r.tip(testAgentBranch), expF)
	require.Equal(t, parentF, out)
	require.Equal(t, ModeNoop, res.Mode, "every conflict resolved to the parent: nothing to commit")
	require.Equal(t, []SettledPath{{Path: expF, Kept: "dst", Dropped: "src-modify", Chosen: true}}, res.Settled)
}

// The overlay is the experiment commit's alone: any other caller of a
// `conflicts` strategy that passes resolutions is still refused.
//
// SABOTAGE S1g (guard relaxed for every site) → no error → red.
func TestConflictsStrategy_ResolutionsStillRefusedElsewhere(t *testing.T) {
	svc, _ := expConflictFork(t, cmOntology("merge"))
	_, err := svc.rh.mergeIntoBranchResolved(context.Background(), "exp/x", testAgentBranch, stratMerge,
		map[string]Resolution{expF: {Side: ResolveSrc}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "computes its own resolutions")
}

// (g) The setting on the EXPERIMENT branch only: an experiment cannot vote
// itself a merge policy. (h) On the parent only: not the consensus branch
// either. Both refused, nothing moved.
//
// SABOTAGE S1h (read at exp.Branch()) → (g) merges → red. SABOTAGE S1i (read
// at exp.Parent) → (h) merges → red.
func TestCommitExperiment_SettingReadAtTheConsensusBranchOnly(t *testing.T) {
	for _, where := range []string{"exp/x", testAgentBranch} {
		t.Run(where, func(t *testing.T) {
			svc, r := expConflictFork(t, "")
			r.write(where, fact.OntologyFile, cmOntology("merge/consensus"))
			before := r.tip(testAgentBranch).Hash
			_, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
			var conflict *MergeConflictError
			require.ErrorAs(t, err, &conflict, "a setting off the consensus branch must not apply")
			require.Contains(t, conflict.Paths, expF)
			require.Equal(t, before, r.tip(testAgentBranch).Hash)
			require.True(t, expStillOpen(t, svc))
		})
	}
}

// (i) `consensus: auto` at the consensus tip with no `conflicts`: the auto
// default (facts: merge, state: consensus) applies, so the commit merges. With
// both keys explicitly off under auto, it is refused.
func TestCommitExperiment_ConsensusAutoDefault(t *testing.T) {
	auto := "id: x\nname: X\nattributes:\n  consensus: auto\n%s" + "topics:\n  notes:\n    description: d\n"
	t.Run("absent conflicts merges", func(t *testing.T) {
		svc, r := expConflictFork(t, strings.Replace(auto, "%s", "", 1))
		res, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
		require.NoError(t, err)
		require.Equal(t, []SettledPath{{Path: expF, Kept: "merged"}}, res.Settled)
		require.Contains(t, r.tip(testAgentBranch).Message, "(conflicts:merge/consensus)")
	})
	t.Run("explicit off refuses", func(t *testing.T) {
		svc, _ := expConflictFork(t, strings.Replace(auto, "%s", "  conflicts:\n    facts: off\n    state: off\n", 1))
		_, err := svc.Experiments().CommitExperiment(context.Background(), "x", nil)
		var conflict *MergeConflictError
		require.ErrorAs(t, err, &conflict)
	})
}
