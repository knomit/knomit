package synthesize

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Tests for the response envelope contract. Every work item ships a
// response_schema whose `required` list names the key its content must arrive
// under — "synthesize" for distill, "decisions" for prune. Nothing enforced
// that list: encoding/json drops unknown keys, so a response that put its
// content under any other name unmarshalled into a zero-valued result, passed
// path validation (which only inspects fields that were never populated), and
// applied as a silent no-op. The item advanced, no error surfaced, and the
// session's summary reported zeros that read as "nothing to do" rather than
// "your facts were discarded".
//
// That is not hypothetical: a real session lost four distilled facts by
// sending {"facts": [...]} — knomit_learn's envelope key — against a distill
// item. Blast radius is any write-bearing step, so prune is pinned too.
//
// The contract these tests fix in place: a MISSING required key is an error;
// a PRESENT but empty one is a legitimate "nothing to do" and must still
// apply cleanly.

// TestReviewer_DistillWrongEnvelopeKey_Rejected is the acceptance test for the
// reported data loss. The payload is the exact shape that was lost.
func TestReviewer_DistillWrongEnvelopeKey_Rejected(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	sess := manualSession(t, svc, branch)
	itemID := insertManualDistillItem(t, svc, sess.ID)

	// The lost payload: content under "facts" instead of "synthesize".
	const wrongKey = `{
		"facts": [{
			"path": "kb/technology/synth.md",
			"title": "S",
			"body": "distilled body",
			"type": "synthesis",
			"domain": ["technology"],
			"confidence": 0.8,
			"entities": [],
			"refs": ["kb/technology/a.md"]
		}],
		"retract": []
	}`

	_, err := r.ContinueSession(ctx, sess.ID, wrongKey)
	require.Error(t, err, "a distill response missing the required \"synthesize\" key must be rejected, not silently dropped")
	require.Contains(t, err.Error(), "synthesize",
		"the error must name the key the agent should have used")
	require.Contains(t, err.Error(), "facts",
		"the error must name the key the agent actually sent, so the fix is obvious")

	require.Empty(t, synthesisFacts(t, svc, branch), "a rejected response must write nothing")

	// Decode runs before the claim, so the item survives for a corrected retry
	// — the whole point of failing loudly rather than consuming the work.
	next, err := svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, next, "a rejected response must leave the item unanswered")
	require.Equal(t, itemID, next.ID)

	_, err = r.ContinueSession(ctx, sess.ID, distillResponseOneFact)
	require.NoError(t, err)
	require.Len(t, synthesisFacts(t, svc, branch), 1,
		"the corrected response must apply normally")
}

// TestReviewer_DistillEmptySynthesize_Accepted is the other half of the
// contract. "I looked and there is nothing worth distilling" is a legitimate
// answer; only an ABSENT key is an error. Without this, the fix above would
// wedge any session whose agent honestly had nothing to synthesize.
func TestReviewer_DistillEmptySynthesize_Accepted(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()

	sess := manualSession(t, svc, "agent/test")
	insertManualDistillItem(t, svc, sess.ID)

	res, err := r.ContinueSession(ctx, sess.ID, `{"synthesize": [], "retract": []}`)
	require.NoError(t, err, "an explicitly empty synthesize array is a valid \"nothing to do\"")
	require.True(t, res.Done)
	require.Empty(t, synthesisFacts(t, svc, "agent/test"))
}

// TestReviewer_PruneWrongEnvelopeKey_Rejected pins the same hole on the other
// write-bearing step. prune's schema requires "decisions"; a response that
// names it anything else retracts nothing and reports success.
func TestReviewer_PruneWrongEnvelopeKey_Rejected(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	const prunePath = "kb/technology/a.md"
	seedObservation(t, svc, branch, prunePath)

	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, prunePath)

	_, err := r.ContinueSession(ctx, sess.ID,
		`{"actions": [{"path": "`+prunePath+`", "action": "retract"}], "merges": []}`)
	require.Error(t, err, "a prune response missing the required \"decisions\" key must be rejected")
	require.Contains(t, err.Error(), "decisions",
		"the error must name the key the agent should have used")

	// The retract never happened, which is exactly what the silent path hid.
	_, rerr := svc.Facts().ReadFact(ctx, branch, prunePath, nil)
	require.NoError(t, rerr, "a rejected prune response must leave the corpus untouched")
}

// TestReviewer_DiscoverWrites_CountedInSummary covers the second defect in the
// same report: applyDiscoverStep never called recordStats, so facts the
// discovery path genuinely wrote were invisible to the session summary. A
// session reporting Synthesized:0 after writing a fact is a false negative on
// a write path — the same class of silent loss, one layer up.
func TestReviewer_DiscoverWrites_CountedInSummary(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	// The proposal's refs must cover every bridge seed, so both must exist.
	seedObservation(t, svc, branch, "kb/technology/a.md")
	seedObservation(t, svc, branch, "kb/technology/b.md")

	sess := manualSession(t, svc, branch)
	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{
				{File: "kb/technology/a.md", Title: "A"},
				{File: "kb/technology/b.md", Title: "B"},
			},
		},
	}
	pj, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "discover",
		ClusterKey: "discover-counted",
		FactsJSON:  string(pj),
		Priority:   1000,
	}))

	res, err := r.ContinueSession(ctx, sess.ID, `{
		"proposals": [{
			"path": "kb/technology/emergent.md",
			"title": "E",
			"body": "an emergent consequence",
			"type": "synthesis",
			"domain": ["technology"],
			"confidence": 0.9,
			"entities": [],
			"refs": ["kb/technology/a.md", "kb/technology/b.md"]
		}]
	}`)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.Len(t, synthesisFacts(t, svc, branch), 1,
		"precondition: the proposal must actually pass the gates and be written")

	require.NotNil(t, res.Summary)
	require.Equal(t, 1, res.Summary.Synthesized,
		"a fact written by the discover path must be counted in the session summary")
}
