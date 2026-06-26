package synthesize

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestReviewer_NormalEffort_DefaultPath confirms the byte-identical-to-prior
// behavior contract for normal effort: NewReviewer (the legacy constructor)
// resolves to EffortNormal, no "discover" work items ever appear on the
// session, and the regular prune/distill items keep flowing.
//
// This guards Plan 03's load-bearing invariant: medium/high opt into
// emergent discovery, normal must NEVER engage it.
func TestReviewer_NormalEffort_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	branch := "agent/test"

	// Seed three distinct epistemic facts so dirtyFacts has something to
	// cluster (the dispatcher needs at least two seeds in a single cluster
	// to produce a prune item; one seed total still yields no work and the
	// session completes immediately).
	for i, slug := range []string{"alpha", "beta", "gamma"} {
		f := fact.NewFact("kb/test/" + slug + ".md")
		f.Title = slug
		f.Body = "body of " + slug
		f.Type = fact.Observation
		f.Domain = []string{"test"}
		f.Confidence = 0.5
		f.Sources = 1
		body, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
		require.NoErrorf(t, err, "seed %d", i)
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	// Legacy constructor — must resolve to normal.
	r := NewReviewer(ri, nil)
	require.Equal(t, EffortNormal, r.Effort(), "NewReviewer must default to EffortNormal")

	res, err := r.StartSession(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	// Session row was created with effort=normal.
	sess, err := svc.Pipeline().GetPipelineSession(context.Background(), res.SessionID)
	require.NoError(t, err)
	require.Equal(t, "normal", sess.Effort, "session must record effort=normal")

	// Walk the queue and assert no "discover" step types ever appear.
	for steps := 0; steps < 50; steps++ {
		if res.Item == nil {
			break
		}
		require.NotEqual(t, "discover", res.Item.Type,
			"normal effort must NEVER enqueue discover work items (got %s)", res.Item.Type)
		// Empty no-op responses pass validation for prune/distill/reflect.
		var noop string
		switch res.Item.Type {
		case "prune":
			noop = `{"decisions":[],"merges":[]}`
		case "distill":
			noop = `{"synthesize":[],"retract":[]}`
		case "reflect":
			noop = `{"methodologies":[]}`
		default:
			t.Fatalf("unexpected step type %q on normal effort", res.Item.Type)
		}
		res, err = r.ContinueSession(context.Background(), res.SessionID, noop)
		require.NoError(t, err)
	}
}

// TestReviewer_HighEffort_PersistedOnSession asserts that the effort field
// round-trips through the session row, so a later ContinueSession can recover
// the dial without re-asking the caller.
func TestReviewer_HighEffort_PersistedOnSession(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})

	r := NewReviewerWithEffort(ri, nil, EffortHigh)
	require.Equal(t, EffortHigh, r.Effort())

	res, err := r.StartSession(context.Background())
	require.NoError(t, err)

	sess, err := svc.Pipeline().GetPipelineSession(context.Background(), res.SessionID)
	require.NoError(t, err)
	require.Equal(t, "high", sess.Effort, "effort high must persist on the session row")
}
