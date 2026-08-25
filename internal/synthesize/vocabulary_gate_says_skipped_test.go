package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// knomit#112. `if d.Effort.MaintainsVocabulary()` gates all four vocabulary
// passes and has NO else. Planning happens once at session start, so a session
// at default (normal) effort plans ZERO motif work items — silently.
//
// The only tell was the ABSENCE of the `motif backfill:` health line, which a
// reader must already know to miss. A population session looped on this doing
// nothing motif-related while looking busy.
//
// This is the failure-shaped-like-success pattern the motif campaign has now
// fixed three times elsewhere (the failed-pass health line; the Evaluated flag
// distinguishing "never asked" from "asked, below floor"; #115's empty return).
// Absence of work must be STATED, never inferred.
//
// MN5 NOTE, verified rather than assumed: this adds a health line at normal
// effort, and the effort-normal invariant's scope is "the DISCOVERY DIMENSION,
// and only that — no normal-vs-medium divergence in what discovery COSTS OR
// PRODUCES". A descriptor line costs no discovery and produces no facts, and
// the invariant explicitly names the general-freeze reading as a MISREADING
// that once blocked a legitimate uniform fix.
func newEffortTestReviewer(t *testing.T, effort Effort) *Reviewer {
	t.Helper()
	const branch = "agent/test"
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	// Enough epistemic facts that Plan has real work — the gate is reached
	// during planning, so a session that completes early never exercises it.
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		f := fact.NewFact("kb/test/" + slug + ".md")
		f.Title = slug
		f.Body = "body of " + slug
		f.Type = fact.Observation
		f.Domain = []string{"test"}
		f.Confidence = 0.5
		f.Sources = 1
		body, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
		require.NoError(t, werr)
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})
	return NewReviewerWithOptions(ri, nil, effort, ScopeFilter{})
}

// At normal effort the gate closes, and the session must SAY the passes were
// skipped and why.
func TestPlan_NormalEffortStatesThatVocabularyPassesWereSkipped(t *testing.T) {
	r := newEffortTestReviewer(t, EffortNormal)

	res, err := r.StartSession(context.Background())
	require.NoError(t, err)

	health := strings.ToLower(strings.Join(res.Health, "\n"))
	require.Contains(t, health, "motif",
		"a session that plans no motif work must say so — the ABSENCE of a "+
			"motif line is not a statement, it is something a reader has to know "+
			"to miss")
	require.Contains(t, health, "skip",
		"and it must name what happened: skipped, not merely missing")
	require.Contains(t, health, "effort",
		"and WHY — the effort dial is the cause, and a reader who does not know "+
			"that cannot act on the line")
}

// At an effort that MAINTAINS the vocabulary the passes run and report for
// themselves, so the skip line must NOT appear. Otherwise the block claims a
// skip that did not happen — the same class of lie the line exists to end.
//
// This is the control, and it is what makes the assertion above mean
// "reported when true" rather than "always printed".
func TestPlan_MediumEffortDoesNotClaimASkip(t *testing.T) {
	r := newEffortTestReviewer(t, EffortMedium)

	res, err := r.StartSession(context.Background())
	require.NoError(t, err)

	health := strings.ToLower(strings.Join(res.Health, "\n"))
	require.NotContains(t, health, "vocabulary passes skipped",
		"the passes RAN at this effort; claiming a skip would be a false "+
			"statement in the same block that reports their results")
}

// The recorder in isolation, so the wording contract is pinned independently of
// how a session happens to be planned. Both halves: it speaks when the gate is
// closed and stays silent when it is open.
func TestRecordVocabularySkipHealth_SpeaksOnlyWhenTheGateIsClosed(t *testing.T) {
	closed := &store.PipelineSession{}
	recordVocabularySkipHealth(closed, EffortNormal)
	require.Len(t, closed.Health, 1, "the closed gate is stated")
	require.Contains(t, closed.Health[0], string(EffortNormal),
		"the line names the effort it was called at, so a reader can tell which "+
			"dial to move")

	open := &store.PipelineSession{}
	recordVocabularySkipHealth(open, EffortMedium)
	require.Empty(t, open.Health,
		"an OPEN gate says nothing here — the passes report for themselves, and "+
			"a skip line beside their output would contradict it")
}
