package store

import (
	"errors"
	"testing"

	"knomit/internal/platform/fileuri"

	"github.com/stretchr/testify/require"
)

// #544 review 3 (X1–X3). Helpers are in remote_origin_snapshot_test.go.

// X1: a round with A merges a foreign commit x into the branch and then fails
// before the snapshot is collapsed. x was ADDED to the snapshot set before
// the reconcile, so after a re-point to B it is class 3: not pushed.
// SABOTAGE: do not add the fetched commit to the set -> x is class 1,
// replayed and pushed to B -> red.
func TestSync_FailureBeforeTheSnapshotWriteStillCoversWhatTheRoundTookIn(t *testing.T) {
	t.Run("new origin a fork from before f", func(t *testing.T) { failedRoundThenRepoint(t, false) })
	// B holds exactly the last success f, so the branch is strictly ahead of
	// B; the rebuild must still collect w above the left-out x.
	// SABOTAGE: take "strictly ahead" under rebuild -> w lost -> red.
	t.Run("new origin holds the last success", func(t *testing.T) { failedRoundThenRepoint(t, true) })
}

func failedRoundThenRepoint(t *testing.T, bHoldsF bool) {
	svc, a, _ := wmFixture(t)
	b := ocCloneBare(t, a)
	f := ocConsumeOwn(t, svc, a)
	if bHoldsF {
		b = ocCloneBare(t, a)
		mustRun(t, b, "git", "update-ref", "-d", "refs/heads/agent/test")
	}
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced")
	x := ocForeignOnBareMain(t, a, "kb/foreign.md", "foreign on A, taken in the failing round")

	once := false
	setStoreHooks(t, storeHooks{beforeOriginSnapshot: func() error {
		return failOnce(&once, errors.New("seam: crash before the snapshot write"))
	}})
	_, err := ocTrySync(svc)
	require.Error(t, err)
	require.True(t, reachableFrom(t, svc, "agent/test", x), "setup: the branch merged x in the failed round")
	require.Greater(t, mustCommit(t, svc, mustHeadHash(t, svc, "agent/test")).NumParents(), 1, "setup: by a merge")
	snap := ocSnapshot(t, svc)
	require.Equal(t, originIdentity(fileuri.New(a)), snap.Identity)
	require.True(t, snap.has(f) && snap.has(x), "the set covers the last success AND what the round took in")

	want := 1 // x; B holds f
	if !bHoldsF {
		ocForeignOnBareMain(t, b, "kb/b-own.md", "B's own main")
		want = 2 // f and x
	}
	ocRepoint(t, svc, fileuri.New(b))
	res := wmSync(t, svc)
	require.Equal(t, want, res.Agent.LeftOut)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"), "w is kept")
	wmPush(t, svc)
	require.False(t, ocBareHasFile(t, b, "agent/test", "kb/foreign.md"), "x does not reach B")
	require.True(t, ocBareHasFile(t, b, "agent/test", "kb/w.md"), "w reaches B")
}

// X1, why a SET: a same-origin scrub round (A resets main to r, removing own
// f) fails after the main rewind, before the agent reconcile; the branch still
// holds f. Had the snapshot been REPLACED by the fetched r, f would read as
// class 1 after a re-point to B and be pushed there.
// SABOTAGE: write the fetched commit alone before the reconcile -> f reaches
// B -> red.
func TestSync_FailedScrubRoundThenOriginChangeDoesNotPushTheScrubbedCommit(t *testing.T) {
	svc, a, r := wmFixture(t)
	b := ocCloneBare(t, a)
	ocConsumeOwn(t, svc, a)
	wmSetOriginMain(t, a, r) // A scrubs f

	once := false
	setStoreHooks(t, storeHooks{afterMainReconcile: func(string) error {
		return failOnce(&once, errors.New("seam: crash after the main rewind"))
	}})
	_, err := ocTrySync(svc)
	require.Error(t, err)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/own.md"), "setup: the branch still holds f")

	ocForeignOnBareMain(t, b, "kb/b-own.md", "B's own main")
	ocRepoint(t, svc, fileuri.New(b))
	wmSync(t, svc)
	wmPush(t, svc)
	require.False(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"), "the scrubbed f does not reach B")
}

// X2: the branch MERGED A's main (own w came before x), so the snapshot is not
// on the branch's first-parent line and the walk's stop point does not catch
// f. Only holding the snapshot's whole history (extraHeld) keeps f and A's
// root out. Both a diverged fork and an unrelated new origin.
// SABOTAGE: unpushedCommitsHeld ignores extraHeld -> f (and A's seed.txt) on
// B -> red.
func TestSync_OriginChangeWithAMergedOldMainHoldsTheSnapshotHistory(t *testing.T) {
	for _, unrelated := range []bool{false, true} {
		name := "diverged fork"
		if unrelated {
			name = "unrelated origin"
		}
		t.Run(name, func(t *testing.T) {
			svc, a, _ := wmFixture(t)
			fork := ocCloneBare(t, a)
			f := ocConsumeOwn(t, svc, a)
			writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced, before x")
			x := ocForeignOnBareMain(t, a, "kb/foreign.md", "foreign on A")
			wmSync(t, svc)
			require.Greater(t, mustCommit(t, svc, mustHeadHash(t, svc, "agent/test")).NumParents(), 1, "setup: the branch merged A's main")
			require.True(t, reachableFrom(t, svc, "agent/test", x))

			b := fork
			if unrelated {
				b = ocUnrelatedBare(t)
			} else {
				ocForeignOnBareMain(t, b, "kb/b-own.md", "B's own main")
			}
			ocRepoint(t, svc, fileuri.New(b))
			res := wmSync(t, svc)
			require.Equal(t, 1, res.Agent.NumReplayed, "only w")
			require.False(t, reachableFrom(t, svc, "agent/test", f))
			wmPush(t, svc)
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/w.md"))
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"), "f does not reach B")
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/foreign.md"), "x does not reach B")
			if unrelated {
				require.False(t, ocBareHasFile(t, b, "agent/test", "seed.txt"), "A's root does not reach B")
			}
		})
	}
}

// X3: the left-out report is derived every round from the kept refs. A round
// that rebuilt and then failed before the snapshot write loses nothing: the
// next, successful round still reports what is left out. When the old origin
// is re-attached and its main brings the commits back, they stop counting.
// SABOTAGE: report only what this round's rebuild left out -> red.
func TestSync_LeftOutReportIsDerivedFromTheKeptRefs(t *testing.T) {
	svc, a, b, _, _ := ocAcross(t, false)
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced")

	once := false
	setStoreHooks(t, storeHooks{beforeOriginSnapshot: func() error {
		return failOnce(&once, errors.New("seam: crash before the snapshot write"))
	}})
	ocRepoint(t, svc, fileuri.New(b))
	_, err := ocTrySync(svc)
	require.Error(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/foreign.md"), "setup: round 1 rebuilt the branch")

	res := wmSync(t, svc)
	require.Equal(t, 2, res.Agent.LeftOut, "f and x, reported after the failed round")
	require.Equal(t, []string{originIdentity(fileuri.New(a))}, res.Agent.LeftOutOrigins)
	require.Len(t, res.Agent.LeftOutRefs, 1)

	ocRepoint(t, svc, fileuri.New(a))
	back := wmSync(t, svc)
	require.Zero(t, back.Agent.LeftOut, "A's main holds f and x again")
	require.Empty(t, back.Agent.LeftOutRefs)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"))
}
