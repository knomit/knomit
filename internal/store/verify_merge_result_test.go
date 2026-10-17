package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// noteConf is a note with a title and a confidence.
func noteConf(title string, conf float64) string {
	f := fact.NewFact("placeholder.md")
	f.Title, f.Confidence, f.Sources, f.Type = title, conf, 1, fact.Observation
	f.Entities = []string{"e"}
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// crossFieldOntology has one cross-field rule: a confident note has a long
// title.
const crossFieldOntology = "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    validations:\n" +
	"      - name: confident-notes-have-long-titles\n        message: m\n        rule: \"fact.confidence < 0.95 || fact.title.length > 3\"\n"

// #533 review 3 B-new, probe P1 made a test: two VALID sides merge, hunk by
// hunk, into an INVALID fact. Main shortens x's title (confidence 0.9: valid);
// the candidate, forked before, raises the confidence to 0.99 (long title:
// valid). The lines are apart, so git's merge is clean and lands confidence
// 0.99 with the title "ab". `verify ci` must judge what the merge lands, not
// only the candidate's tip.
//
// SABOTAGE: skip mergeResultValid -> exit 0 -> red.
func TestValidation_TwoValidSidesThatMergeIntoAnInvalidFactAreRefused(t *testing.T) {
	f := newRangeFixture(t)
	base := map[string]string{fact.OntologyFile: crossFieldOntology}
	R := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("a long title", 0.9)), "", nil)
	mainC := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("ab", 0.9)), "agent-m", nil, R)
	f.ref(f.kb, "main", mainC)
	T := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("a long title", 0.99)), "agent-x", nil, R)
	f.ref(f.kb, "cand", T)

	v, err := CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Equal(t, 1, v.ExitCode(), "the merge lands an invalid fact: %+v", v.Refused)
	require.Len(t, v.Refused, 1)
	require.Equal(t, RuleValidation, v.Refused[0].Rule)
	require.Equal(t, T.Hash.String(), v.Refused[0].Commit)
	require.Contains(t, v.Refused[0].Reason, "kb/notes/x.md")
	require.Contains(t, v.Refused[0].Reason, "confident-notes-have-long-titles")
	require.Contains(t, v.Refused[0].Reason, "merging the branch into main")

	// Control: once the branch brings main in and fixes the title, the merge
	// is the branch's tip and it is admitted.
	fixed := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("a fine title", 0.99)), "agent-x", nil, T, mainC)
	f.ref(f.kb, "cand", fixed)
	v, err = CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Equal(t, 0, v.ExitCode(), "%+v", v.Refused)
}

// #533 review 3: a candidate whose merge into main conflicts is not admitted
// (RuleMergeConflict, blocking in every mode); a candidate behind main that
// merges cleanly into valid content is admitted.
//
// SABOTAGE: treat a conflicted merge-tree as clean -> the conflict row is
// admitted -> red.
func TestValidation_ACandidateThatConflictsWithMainIsRefused(t *testing.T) {
	f := newRangeFixture(t)
	base := map[string]string{fact.OntologyFile: crossFieldOntology}
	R := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("a long title", 0.9)), "", nil)
	mainC := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("main's long title", 0.9)), "agent-m", nil, R)
	f.ref(f.kb, "main", mainC)

	// Clean and valid: the branch adds another fact.
	ok := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("a long title", 0.9), "kb/notes/y.md", noteConf("why", 0.5)), "agent-x", nil, R)
	f.ref(f.kb, "cand", ok)
	v, err := CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Equal(t, 0, v.ExitCode(), "%+v", v.Refused)

	// Conflict: both sides rewrite the title line.
	T := f.commit(f.kb, with(base, "kb/notes/x.md", noteConf("the branch's long title", 0.9)), "agent-x", nil, R)
	f.ref(f.kb, "cand", T)
	v, err = CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Equal(t, 1, v.ExitCode(), "%+v", v.Refused)
	require.Len(t, v.Refused, 1)
	require.Equal(t, RuleMergeConflict, v.Refused[0].Rule)
	require.Equal(t, T.Hash.String(), v.Refused[0].Commit)
}
