package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// gateFixture is a real Service whose origin/main is advanced by writing
// commits straight into its object store (the store tests' "pretend a fetch
// happened"), then reconciled through reconcileNow.
type gateFixture struct {
	*foldFixture
	svc  *Service
	init *object.Commit // the local main at start (the repo's init commit)
}

const gateAgent = "agent/test"

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	f := newFoldFixture(t)
	svc, err := Open(filepath.Join(t.TempDir(), "g.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, gateAgent))
	svc.SetRootOfTrust(f.root)
	svc.SetSigner(f.a) // the local instance is agent a (reconcile merges are signed)
	f.r = repoOn(t, svc.rh.gits)
	init, err := svc.rh.repo.CommitObject(mustHeadHash(t, svc, "main"))
	require.NoError(t, err)
	return &gateFixture{foldFixture: f, svc: svc, init: init}
}

func (g *gateFixture) setOrigin(c *object.Commit) {
	g.t.Helper()
	require.NoError(g.t, g.svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), c.Hash)))
}

func (g *gateFixture) reconcile() SyncResult {
	g.t.Helper()
	res, err := g.svc.Remote().(*remoteIndex).reconcileNow(context.Background(), gateAgent, "main")
	require.NoError(g.t, err)
	return res
}

func (g *gateFixture) anchor() plumbing.Hash {
	ref, err := g.svc.rh.gits.Reference(verifiedRefName("main"))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash
	}
	require.NoError(g.t, err)
	return ref.Hash()
}

func (g *gateFixture) contains(tip, c plumbing.Hash) bool {
	g.t.Helper()
	ok, err := isAncestorCommit(g.svc.rh, c, tip)
	require.NoError(g.t, err)
	return ok
}

// history returns init → operator enable(mode) → good(a) → unsigned → stranger.
func (g *gateFixture) history(mode string) (enable, good, unsigned, stranger *object.Commit) {
	files := with(g.baseFiles, g.ontPath, g.ont(mode, g.a, g.b))
	enable = g.commit(files, g.op, g.init)
	good = g.commit(with(files, "kb/good.md", "g"), g.a, enable)
	unsigned = g.commit(with(files, "kb/good.md", "g", "kb/unsigned.md", "u"), nil, good)
	stranger = g.commit(with(files, "kb/good.md", "g", "kb/unsigned.md", "u", "kb/stranger.md", "s"), g.stranger, unsigned)
	return
}

// TestGate_EnforceHoldsAtTheLastGoodCommit: proposal test 1 (R1 as a test).
// Refs advance up to the last good commit and stop there; the agent branch
// never receives the refused commits.
func TestGate_EnforceHoldsAtTheLastGoodCommit(t *testing.T) {
	g := newGateFixture(t)
	_, good, unsigned, stranger := g.history(VerifyEnforce)
	g.setOrigin(stranger)

	res := g.reconcile()
	require.NotNil(t, res.Main.Verify)
	require.True(t, res.Main.Verify.Blocking(), "enforce must block")
	require.True(t, res.Main.Verify.Held)
	require.Len(t, res.Main.Verify.Refused, 2)
	require.Equal(t, good.Hash, mustHeadHash(t, g.svc, "main"), "local main stops at the last good commit")
	require.Equal(t, good.Hash, g.anchor())
	agentTip := mustHeadHash(t, g.svc, gateAgent)
	require.False(t, g.contains(agentTip, unsigned.Hash), "the agent branch must not receive a refused commit")
	require.True(t, g.contains(agentTip, good.Hash), "the agent branch receives what verified")
}

// TestGate_LogMovesButTheAnchorStays: in log the upstream moves to origin, the
// anchor stops before the first failure, and the report repeats every tick.
func TestGate_LogMovesButTheAnchorStays(t *testing.T) {
	g := newGateFixture(t)
	_, good, _, stranger := g.history(VerifyLog)
	g.setOrigin(stranger)

	for tick := 1; tick <= 3; tick++ {
		res := g.reconcile()
		require.NotNil(t, res.Main.Verify, "tick %d", tick)
		require.False(t, res.Main.Verify.Blocking(), "log never blocks")
		require.Len(t, res.Main.Verify.Refused, 2, "tick %d: the failure is re-reported while the anchor stays", tick)
		require.Equal(t, stranger.Hash, mustHeadHash(t, g.svc, "main"), "log moves the upstream to origin")
		require.Equal(t, good.Hash, g.anchor(), "tick %d: the anchor never passes a failing commit", tick)
	}
}

// TestGate_OffIsUntouched: proposal test 15 and the plan's "off by default,
// provably": absent AND explicit off, both reconcile exactly as before, with no
// signature or M3 evaluation, no report and no anchor ref.
func TestGate_OffIsUntouched(t *testing.T) {
	for name, ont := range map[string]func(*gateFixture) string{
		"absent":       func(g *gateFixture) string { return g.ont("") },
		"explicit off": func(g *gateFixture) string { return g.ont(VerifyOff) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newGateFixture(t)
			files := with(g.baseFiles, g.ontPath, ont(g))
			c1 := g.commit(files, nil, g.init)
			c2 := g.commit(with(files, "kb/x.md", "x"), g.stranger, c1)
			g.setOrigin(c2)
			res := g.reconcile()
			require.Nil(t, res.Main.Verify, "an off repo has nothing to report")
			require.Equal(t, ModeFF, res.Main.Mode)
			require.Equal(t, c2.Hash, mustHeadHash(t, g.svc, "main"))
			require.Equal(t, plumbing.ZeroHash, g.anchor(), "off never writes the anchor ref")
		})
	}
}

// TestGate_ObjectsPresentAreNotTrusted: proposal test 8. The refused commits'
// objects stay in the store after a refused tick; the next tick refuses again.
func TestGate_ObjectsPresentAreNotTrusted(t *testing.T) {
	g := newGateFixture(t)
	_, good, _, stranger := g.history(VerifyEnforce)
	g.setOrigin(stranger)
	first := g.reconcile()
	second := g.reconcile()
	require.True(t, first.Main.Verify.Blocking())
	require.True(t, second.Main.Verify.Blocking(), "a second tick must refuse again")
	require.Len(t, second.Main.Verify.Refused, 2)
	require.Equal(t, good.Hash, mustHeadHash(t, g.svc, "main"))
}

// TestGate_RewindHoldsInEnforce: proposal test 6a. Origin force-moved off the
// anchor: enforce holds the upstream and the anchor; nothing moves.
func TestGate_RewindHoldsInEnforce(t *testing.T) {
	g := newGateFixture(t)
	enable, good, _, _ := g.history(VerifyEnforce)
	g.setOrigin(good)
	g.reconcile()
	require.Equal(t, good.Hash, g.anchor())

	sibling := g.commit(with(g.baseFiles, g.ontPath, g.ont(VerifyEnforce, g.a, g.b), "kb/other.md", "o"), g.a, enable)
	g.setOrigin(sibling)
	res := g.reconcile()
	require.True(t, res.Main.Verify.Rewind)
	require.True(t, res.Main.Verify.Blocking())
	require.Equal(t, good.Hash, mustHeadHash(t, g.svc, "main"), "a rewind past the anchor holds the upstream in enforce")
	require.Equal(t, good.Hash, g.anchor(), "and never moves the anchor")
}

// TestGate_LogFailureRefusedAfterEnforce: proposal test 10. A stranger's
// commit in log moves main and leaves the anchor behind; a later operator
// tightening to enforce refuses the same upstream, and main is never moved
// backwards.
func TestGate_LogFailureRefusedAfterEnforce(t *testing.T) {
	g := newGateFixture(t)
	_, good, _, stranger := g.history(VerifyLog)
	g.setOrigin(stranger)
	g.reconcile()
	require.Equal(t, stranger.Hash, mustHeadHash(t, g.svc, "main"))

	tighten := g.commit(with(g.baseFiles, g.ontPath, g.ont(VerifyEnforce, g.a, g.b),
		"kb/good.md", "g", "kb/unsigned.md", "u", "kb/stranger.md", "s"), g.op, stranger)
	g.setOrigin(tighten)
	res := g.reconcile()
	require.True(t, res.Main.Verify.Blocking(), "the log-mode failures are refused once enforce arrives")
	require.Equal(t, stranger.Hash, mustHeadHash(t, g.svc, "main"), "never backwards, and not forward past the failure")
	require.Equal(t, good.Hash, g.anchor())
}

// TestGate_CachedHistoryAgreesWithAFreshFold: the second tick uses the cached
// anchor history (verifyBelow); its answer must equal a root fold of the same
// tip by a fresh verifier.
func TestGate_CachedHistoryAgreesWithAFreshFold(t *testing.T) {
	g := newGateFixture(t)
	files := with(g.baseFiles, g.ontPath, g.ont(VerifyEnforce, g.a, g.b))
	e := g.commit(files, g.op, g.init)
	good1 := g.commit(with(files, "kb/1.md", "1"), g.a, e)
	g.setOrigin(good1)
	g.reconcile()
	require.NotNil(t, g.svc.rh.cachedBelow("main", good1.Hash), "the first tick must seed the cache")

	good2 := g.commit(with(files, "kb/1.md", "1", "kb/2.md", "2"), g.b, good1)
	bad := g.commit(with(files, "kb/1.md", "1", "kb/2.md", "2", "kb/3.md", "3"), nil, good2)
	g.setOrigin(bad)
	res := g.reconcile()
	require.Equal(t, good2.Hash, g.anchor())
	require.Len(t, res.Main.Verify.Refused, 1)

	fresh, err := (&verifier{st: g.svc.rh.gits, root: g.root}).fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, bad.Hash)
	require.NoError(t, err)
	require.Equal(t, fresh.NewAnchor, g.anchor(), "cached and fresh folds must agree on the anchor")
	require.Equal(t, refusedSet(fresh), refusedSet(foldResult{Refused: res.Main.Verify.Refused}))
}
