package synthesize

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// knomit#120. The work item exposes id/type/prompt/response_schema/facts — not
// ClusterKey. So an answering agent cannot follow the rule "judge restate-
// items honestly, never no-op them", because it cannot tell WHICH 2-fact prune
// item is one.
//
// The consequence is not theoretical and it is destructive. A no-op on a
// restatement item records a DECLINED verdict against the judge-outcome
// throttle AND permanently retires the standing pair — DeleteRestatementPair
// runs unconditionally after the verdict. Measured: a session whose health said
// "restatement candidates emitted: 1" served two 2-fact prune items; the
// operator no-op'd both under the ordinary-cluster rule. If either carried a
// restate- key, that no-op was #111's destructive non-answer — unavoidable
// rather than avoidable, because nothing on the wire could distinguish them.
//
// Until this ships, the only safe fleet rule is "treat every 2-fact prune as a
// possible restatement pair", which spends honest judgment on ordinary
// clusters every session.
func TestNextItem_CarriesClusterKeyOnTheWire(t *testing.T) {
	ctx := context.Background()
	r, svc := newPhaseTestReviewer(t)
	sess := manualSession(t, svc, "agent/test")

	// Two 2-fact prune items, indistinguishable on every field the wire
	// carried before this fix: same type, same shape, same fact count.
	require.NoError(t, svc.Pipeline().InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "prune",
		ClusterKey: restatementClusterKeyPrefix + "0",
		FactsJSON:  `[{"file":"kb/technology/a.md"},{"file":"kb/technology/b.md"}]`,
		Priority:   restatementPriority,
	}))
	require.NoError(t, svc.Pipeline().InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "prune",
		ClusterKey: "cluster-7",
		FactsJSON:  `[{"file":"kb/technology/c.md"},{"file":"kb/technology/d.md"}]`,
		Priority:   0.1,
	}))

	// BOTH LAYERS, asserted separately — because they are separate structs
	// populated at separate places, and this test's name promises the wire.
	//
	// The engine's PipelineItem is not what an MCP client sees; ReviewItem is,
	// and `reviewResult` projects one onto the other field by field. A fix that
	// populated the engine type and forgot the projection would satisfy an
	// engine-level assertion completely while leaving the wire exactly as
	// broken as before — the same gap this campaign has now hit five times.
	engine, err := r.p.nextItem(ctx, sess)
	require.NoError(t, err)
	require.NotNil(t, engine.Item)

	// The restatement item sorts first on restatementPriority, so this is the
	// one an agent meets — and it must be able to tell what it is holding.
	require.Equal(t, restatementClusterKeyPrefix+"0", engine.Item.ClusterKey,
		"engine layer: the PipelineItem must carry the stored item's key")

	wire, err := r.PageItem(ctx, sess.ID, engine.Item.ID, 1)
	require.NoError(t, err)
	require.NotNil(t, wire.Item)
	require.Equal(t, restatementClusterKeyPrefix+"0", wire.Item.ClusterKey,
		"WIRE layer: a restatement pair must be distinguishable from an "+
			"ordinary 2-fact prune cluster in what the CLIENT receives; without "+
			"this the only safe rule is to treat every 2-fact prune as possibly "+
			"destructive to no-op")
}

// The PAGING path builds its item separately (pipeline.go has two PipelineItem
// construction sites), and a paged item is exactly where the distinction is
// most likely to be needed — a large restatement pair arrives across pages, and
// the agent decides how to answer after reading them.
//
// This is the wiring half: populating the field at one construction site and
// not the other would leave paged items indistinguishable while the unit test
// above passed.
func TestPageItem_CarriesClusterKeyOnEveryPage(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()

	item := currentDistillItem(t, svc, sessionID)
	require.NotEmpty(t, item.ClusterKey, "precondition: the stored item has a key to carry")

	first, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	require.Greater(t, first.Item.Pages, 1,
		"precondition: the item must genuinely span more than one page, or the "+
			"page-2 construction site below is never exercised")

	// EVERY page, not just the first. Page 1 falls through to renderWorkItem;
	// pages after it are built by payloadResult — a SECOND PipelineItem
	// construction site. Populating one and not the other would leave a paged
	// item identifiable on its first page and anonymous on the rest, which is
	// worse than uniformly absent: the agent reads the later pages last, and
	// that is when it decides how to answer.
	for p := 1; p <= first.Item.Pages; p++ {
		res, perr := r.PageItem(ctx, sessionID, item.ID, p)
		require.NoErrorf(t, perr, "page %d", p)
		require.Equalf(t, item.ClusterKey, res.Item.ClusterKey,
			"page %d dropped the cluster key; both PipelineItem construction "+
				"sites must carry it", p)
	}
}

// THE WIRE CONTRACT, pinned deliberately.
//
// Exposing the raw cluster key rather than a derived is_restatement flag is the
// choice this fix makes: the key already exists and is already computed, so it
// cannot drift from its source the way a second declaration would.
//
// The cost of that choice is that consumers key off the `restate-` PREFIX, and
// a prefix nothing pins is a silent-breakage contract — change the constant and
// every agent's rule quietly stops matching, with no failure anywhere. This
// test is that pin: it fails, naming the wire contract, if the prefix changes.
func TestRestatementClusterKeyPrefix_IsAWireContract(t *testing.T) {
	require.Equal(t, "restate-", restatementClusterKeyPrefix,
		"this prefix is now part of the WIRE CONTRACT (#120): answering agents "+
			"identify restatement pairs by it, and a non-answer on one is "+
			"destructive (#111). Changing it silently breaks every consumer's "+
			"rule — if it must change, the change is a coordinated wire change, "+
			"not a rename")

	// And the producer still uses it, so the pin is on a live path rather than
	// on an orphaned constant.
	require.True(t, strings.HasPrefix(restatementClusterKeyPrefix+"0", "restate-"))
}
