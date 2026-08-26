package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Phase-2 multi-session dynamics.
//
// The standing requirement exists because the Phase-0 review found every
// component unit-correct and every blocking defect in the CROSS-SESSION
// behaviour. Single-session tests structurally cannot see this failure class:
// a vocabulary that resolves correctly once, an alias table that rebuilds
// correctly once, and a definition that is authored correctly once can all
// still be wrong the second time a session runs over them.
//
// Every test here runs 3+ sessions and asserts something only their SEQUENCE
// can establish.

// vocabSession runs one review session at an effort that maintains the
// vocabulary. runSession uses EffortNormal, where all three motif passes are
// deliberately skipped (MN5), so it cannot exercise any of this.
func (e *restatementEnv) vocabSession() sessionOutcome {
	e.t.Helper()
	res, err := NewReviewerWithOptions(e.ri, nil, EffortMedium, ScopeFilter{}).StartSession(context.Background())
	require.NoError(e.t, err)
	out := sessionOutcome{sessionID: res.SessionID, health: res.Health}
	for _, item := range e.workItems(res.SessionID) {
		out.restatementItems = append(out.restatementItems, item)
	}
	return out
}

// itemsOfType returns this session's work items of one step type.
func (o sessionOutcome) itemsOfType(step string) []store.PipelineWorkItem {
	var out []store.PipelineWorkItem
	for _, i := range o.restatementItems {
		if i.StepType == step {
			out = append(out, i)
		}
	}
	return out
}

// A canonical id must survive the vocabulary GROWING around it. A corpus that
// re-elected a different representative every session would break every
// consumer that stored one — and the cluster key, which definitions hang off,
// must not move at all.
func TestPhase2Dynamics_ClusterIdentitySurvivesAGrowingVocabulary(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// "write-atomic" leads on df, so the representative NAME differs from the
	// cluster KEY. Without that difference the test cannot tell them apart —
	// the first version used a cluster whose name and key were the same string,
	// and passed with cluster_key sabotaged to follow the representative.
	env.writeFactWithMotifs("kb/a.md", "Alpha", "body a", []string{"write-atomic"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "body b", []string{"write-atomic"})
	env.writeFactWithMotifs("kb/c.md", "Charlie", "body c", []string{"atomic-write"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	keyBefore, err := env.svc.Motifs().ClusterKey(ctx, env.branch, "write-atomic")
	require.NoError(t, err)
	repBefore, err := env.svc.Motifs().CanonicalID(ctx, env.branch, "write-atomic")
	require.NoError(t, err)
	require.Equal(t, "write-atomic", repBefore)
	require.NotEqual(t, repBefore, keyBefore,
		"precondition: name and key must differ, or this test cannot tell them apart")

	// Session 2: unrelated vocabulary grows around the cluster.
	env.writeFactWithMotifs("kb/n1.md", "New one", "body n1", []string{"unrelated-alpha-shape"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))
	keyNow, err := env.svc.Motifs().ClusterKey(ctx, env.branch, "write-atomic")
	require.NoError(t, err)
	require.Equal(t, keyBefore, keyNow, "unrelated vocabulary must not move a cluster key")

	// Session 3: usage shifts INSIDE the cluster, flipping the representative.
	// This is the case that distinguishes the two identities, and the one a
	// definition keyed to the representative would be orphaned by.
	env.writeFactWithMotifs("kb/d.md", "Delta", "body d", []string{"atomic-write"})
	env.writeFactWithMotifs("kb/e.md", "Echo", "body e", []string{"atomic-write"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	repAfter, err := env.svc.Motifs().CanonicalID(ctx, env.branch, "write-atomic")
	require.NoError(t, err)
	require.NotEqual(t, repBefore, repAfter,
		"precondition: the fixture must actually flip the representative")

	keyAfter, err := env.svc.Motifs().ClusterKey(ctx, env.branch, "write-atomic")
	require.NoError(t, err)
	require.Equal(t, keyBefore, keyAfter,
		"the cluster KEY must not move when the representative does — across sessions, "+
			"which is where a definition or a verdict would silently lose its anchor")

	clusters, err := env.svc.Motifs().Clusters(ctx, env.branch)
	require.NoError(t, err)
	require.Len(t, clusters, 2, "one aliased cluster plus the unrelated one")
}

// A judge decision must not need re-authorising every session — that is what
// makes the pass incremental — but it must STOP binding when either cluster's
// membership moves, because the judge was answering about a different cluster.
func TestPhase2Dynamics_VerdictBindsUntilMembershipMoves(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha", "body a", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "body b", []string{"config-drift"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))
	require.NoError(t, env.svc.Motifs().RecordJudgeDecline(ctx, env.branch,
		"silent-fallback", "config-drift"))

	keyA, err := env.svc.Motifs().ClusterKey(ctx, env.branch, "silent-fallback")
	require.NoError(t, err)
	keyB, err := env.svc.Motifs().ClusterKey(ctx, env.branch, "config-drift")
	require.NoError(t, err)
	want := pairKeyForTest(keyA, keyB)

	// Two further sessions change nothing about either cluster. The verdict
	// must keep binding, or the pass re-litigates settled questions forever.
	for i := range 2 {
		env.writeFact(fmt.Sprintf("kb/noise%d.md", i), "Noise", "unrelated body")
		require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))
		answered, err := env.svc.Motifs().AnsweredPairs(ctx, env.branch)
		require.NoError(t, err)
		require.Containsf(t, answered, want, "session %d let a settled verdict lapse", i)
	}

	// Now a new spelling joins one cluster. It covers more than the judge saw.
	env.writeFactWithMotifs("kb/c.md", "Charlie", "body c", []string{"silent-fallbacks"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))
	answered, err := env.svc.Motifs().AnsweredPairs(ctx, env.branch)
	require.NoError(t, err)
	require.NotContains(t, answered, want,
		"a cluster that gained a member is not the cluster the judge declined")
}

// Definitions must be authored ONCE and refreshed only when their cluster's
// membership moves. A pass that re-authored every session would pay an LLM to
// restate the same sentence forever.
func TestPhase2Dynamics_DefinitionsAreAuthoredOnceAndRefreshedOnChange(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha", "body a", []string{"silent-fallback"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	key, err := env.svc.Motifs().ClusterKey(ctx, env.branch, "silent-fallback")
	require.NoError(t, err)
	require.NoError(t, env.svc.Motifs().PutDefinition(ctx, env.branch, key, "A generic sentence.", store.DefinitionStamp{}))

	// Sessions 2 and 3: unrelated corpus growth. The definition must stay off
	// the queue.
	for i := range 2 {
		env.writeFact(fmt.Sprintf("kb/noise%d.md", i), "Noise", "unrelated body")
		require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))
		need, err := env.svc.Motifs().ClustersNeedingDefinition(ctx, env.branch)
		require.NoError(t, err)
		for _, c := range need {
			require.NotEqualf(t, key, c.ClusterKey,
				"session %d re-queued a definition nothing changed", i)
		}
	}

	// Session 4: the cluster gains a spelling. Now it must be re-queued — and
	// must KEEP its interim sentence rather than gapping.
	env.writeFactWithMotifs("kb/b.md", "Bravo", "body b", []string{"silent-fallbacks"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	need, err := env.svc.Motifs().ClustersNeedingDefinition(ctx, env.branch)
	require.NoError(t, err)
	var queued *store.DefinitionTarget
	for i := range need {
		if need[i].ClusterKey == key {
			queued = &need[i]
		}
	}
	require.NotNil(t, queued, "a cluster that gained a member must be re-queued")
	require.Equal(t, "A generic sentence.", queued.Interim,
		"...carrying its interim definition, so the cluster is never left gapped")

	def, ok, err := env.svc.Motifs().Definition(ctx, env.branch, key)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEmpty(t, def, "the standing definition is still served while stale")
}

// Backfill coverage must CLOSE over sessions. A bounded pass that re-offered
// the same head every session would never reach the tail.
// A motif written in session N must survive the machinery of session N+1 —
// the alias rebuild and the definition pass.
//
// This test used to drive that machinery through the backfill pass, including
// an assertion that backfill refused to overwrite an existing motif. Backfill
// is gone; the property it was demonstrating is not. What remains is the part
// that never depended on backfill: run the vocabulary sessions that DO still
// exist and assert an authored motif is neither changed nor rewritten. The
// blob-hash assertion is the load-bearing half — a pass that rewrote the fact
// to an identical value would still be touching authored data.
func TestPhase2Dynamics_AuthoredMotifSurvivesLaterSessions(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/authored.md", "Authored", "body", []string{"silent-fallback"})
	env.writeFact("kb/bare.md", "Bare", "body")
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	before, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/authored.md")
	require.NoError(t, err)

	for range 3 {
		env.vocabSession()
		require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))
	}

	after, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/authored.md")
	require.NoError(t, err)
	require.Equal(t, []string{"silent-fallback"}, after.Motifs,
		"an authored motif must survive every later session untouched")
	require.Equal(t, before.BlobHash, after.BlobHash,
		"...and the fact must not even be rewritten")
}

// A session's health must report every subsystem that ran. Losing one set of
// lines makes a broken subsystem indistinguishable from a clean corpus.
func TestPhase2Dynamics_HealthReportsEverySubsystemEverySession(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	for i := range 6 {
		env.writeFactWithMotifs(fmt.Sprintf("kb/f%d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("body %d", i), []string{"silent-fallback"})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	for session := range 3 {
		out := env.vocabSession()
		joined := strings.Join(out.health, "\n")
		for _, want := range []string{
			"abstraction coverage",       // phase 0
			"standing restatement pairs", // phase 0
			"motif signal",               // phase 2, §7
			"motif vocabulary",           // phase 2, §3.3
		} {
			require.Containsf(t, joined, want,
				"session %d lost the %q health line — a subsystem that reports nothing "+
					"is indistinguishable from one that found nothing", session, want)
		}
		env.writeFact(fmt.Sprintf("kb/extra%d.md", session), "Extra", "body")
	}
}

// pairKeyForTest mirrors the store's unordered pair identity.
func pairKeyForTest(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "\x00" + b
}
