// Category C addendum — ref navigation through a full lifecycle.
//
// Builds a graph of facts connected by refs, then mutates it with
// retraction and subsumption. Asserts that temporal navigation via
// FollowRef lets you walk the full chain at any historical snapshot,
// even after later mutations break individual links at HEAD.
package storytests

import (
	"testing"

	"knomit/internal/testenv"
)

// TestRefNavigation_FullLifecycle builds a six-fact graph over nine
// commits and asserts that FollowRef resolves correctly at every
// interesting time slice:
//
//	c1: alpha            (root fact)
//	c2: beta             (root fact)
//	c3: gamma → [alpha, beta]
//	c4: delta → [beta]
//	c5: epsilon → [beta]
//	c6: retract beta
//	c7: zeta → [delta, epsilon]   (subsume delta+epsilon)
//	c8: delete delta
//	c9: delete epsilon
//
// At c5 the full graph is alive and every link resolves.
// At c7 zeta can reach delta and epsilon, but beta is already gone.
// At c9 (HEAD) only alpha, gamma, and zeta survive; every deeper
// FollowRef hits a broken link. Temporal navigation back to c5
// recovers the full chain.
func TestRefNavigation_FullLifecycle(t *testing.T) {
	t.Log("C8: six-fact ref graph — retract + subsume; temporal navigation recovers full chain")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	// ── build the graph ──────────────────────────────────────────────
	c1 := agent.Write("kb/alpha.md", testenv.Fact("alpha").Confidence(0.9), "init alpha")
	c2 := agent.Write("kb/beta.md", testenv.Fact("beta").Confidence(0.8), "init beta")
	c3 := agent.Write("kb/gamma.md",
		testenv.Fact("gamma").Refs("kb/alpha.md", "kb/beta.md"),
		"gamma refs alpha+beta")
	c4 := agent.Write("kb/delta.md",
		testenv.Fact("delta").Refs("kb/beta.md"),
		"delta refs beta")
	c5 := agent.Write("kb/epsilon.md",
		testenv.Fact("epsilon").Refs("kb/beta.md"),
		"epsilon refs beta")

	// ── retract beta ─────────────────────────────────────────────────
	c6 := agent.Delete("kb/beta.md", "retract beta")

	// ── subsume delta+epsilon into zeta ──────────────────────────────
	c7 := agent.Write("kb/zeta.md",
		testenv.Fact("zeta").Refs("kb/delta.md", "kb/epsilon.md"),
		"zeta subsumes delta+epsilon")
	agent.Delete("kb/delta.md", "remove subsumed delta")
	c9 := agent.Delete("kb/epsilon.md", "remove subsumed epsilon")
	_ = c9

	// ── assertions ───────────────────────────────────────────────────

	// 1. At c1: only alpha exists.
	c1.Fact("kb/alpha.md").MustExist()
	c1.Fact("kb/beta.md").MustNotExist()

	// 2. At c2: alpha and beta both exist.
	c2.Fact("kb/alpha.md").MustExist()
	c2.Fact("kb/beta.md").MustExist()

	// 3. At c3: gamma can reach both roots.
	c3.Fact("kb/gamma.md").FollowRef("kb/alpha.md").MustExist()
	c3.Fact("kb/gamma.md").FollowRef("kb/alpha.md").Confidence().MustEqual(0.9)
	c3.Fact("kb/gamma.md").FollowRef("kb/beta.md").MustExist()
	c3.Fact("kb/gamma.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)

	// 4. At c5 the full graph is alive. Walk every path:
	//    gamma → alpha          ✓
	//    gamma → beta           ✓
	//    delta → beta           ✓
	//    epsilon → beta         ✓
	c5.Fact("kb/gamma.md").FollowRef("kb/alpha.md").MustExist()
	c5.Fact("kb/gamma.md").FollowRef("kb/beta.md").MustExist()
	c5.Fact("kb/delta.md").FollowRef("kb/beta.md").MustExist()
	c5.Fact("kb/delta.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)
	c5.Fact("kb/epsilon.md").FollowRef("kb/beta.md").MustExist()

	// 5. At c4 (before epsilon): delta→beta OK, epsilon doesn't exist yet.
	c4.Fact("kb/delta.md").FollowRef("kb/beta.md").MustExist()
	c4.Fact("kb/epsilon.md").MustNotExist()

	// 6. At c6 (beta retracted): every link TO beta is now broken,
	//    but the upstream facts still exist.
	c6.Fact("kb/gamma.md").MustExist()
	c6.Fact("kb/gamma.md").FollowRef("kb/alpha.md").MustExist()
	c6.Fact("kb/gamma.md").FollowRef("kb/beta.md").MustBeBroken()
	c6.Fact("kb/delta.md").FollowRef("kb/beta.md").MustBeBroken()
	c6.Fact("kb/epsilon.md").FollowRef("kb/beta.md").MustBeBroken()

	// 7. At c7 (zeta written, delta+epsilon still alive):
	//    zeta → delta           ✓
	//    zeta → epsilon         ✓
	//    delta → beta           ✗ broken (retracted at c6)
	//    epsilon → beta         ✗ broken
	c7.Fact("kb/zeta.md").FollowRef("kb/delta.md").MustExist()
	c7.Fact("kb/zeta.md").FollowRef("kb/epsilon.md").MustExist()
	c7.Fact("kb/zeta.md").FollowRef("kb/delta.md").FollowRef("kb/beta.md").MustBeBroken()
	c7.Fact("kb/zeta.md").FollowRef("kb/epsilon.md").FollowRef("kb/beta.md").MustBeBroken()

	// 8. At HEAD (c9): delta and epsilon are gone too.
	head := agent.Head()
	head.Fact("kb/alpha.md").MustExist()
	head.Fact("kb/beta.md").MustNotExist()
	head.Fact("kb/gamma.md").MustExist()
	head.Fact("kb/delta.md").MustNotExist()
	head.Fact("kb/epsilon.md").MustNotExist()
	head.Fact("kb/zeta.md").MustExist()
	head.Fact("kb/zeta.md").FollowRef("kb/delta.md").MustBeBroken()
	head.Fact("kb/zeta.md").FollowRef("kb/epsilon.md").MustBeBroken()
	// gamma → alpha still works even at HEAD.
	head.Fact("kb/gamma.md").FollowRef("kb/alpha.md").MustExist()
	head.Fact("kb/gamma.md").FollowRef("kb/alpha.md").Confidence().MustEqual(0.9)
	// gamma → beta is broken at HEAD.
	head.Fact("kb/gamma.md").FollowRef("kb/beta.md").MustBeBroken()

	// 9. Temporal recovery: go back to c5 where everything was alive.
	//    Walk the deepest chain: gamma → beta → (beta is a leaf, no
	//    outgoing refs). delta → beta → leaf. All resolve.
	//    This is the payoff: even though HEAD is full of broken links,
	//    temporal navigation back to c5 recovers the full graph.
	c5.Fact("kb/gamma.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)
	c5.Fact("kb/delta.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)
	c5.Fact("kb/epsilon.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)
}
