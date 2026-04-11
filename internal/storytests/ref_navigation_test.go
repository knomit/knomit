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
//	c1: alpha                        (root fact)
//	c2: beta                         (root fact)
//	c3: gamma → [alpha, beta]
//	c4: retract beta
//	c5: delta                        (new leaf)
//	c6: epsilon → [gamma, delta]
//	c7: retract delta
//	c8: zeta → [gamma, epsilon]      (subsumes gamma + epsilon)
//	    delete gamma
//	c9: delete epsilon
//
// The deepest chain at c6 is: epsilon → gamma → alpha (3 hops).
// At HEAD only alpha and zeta survive as live facts. Temporal
// navigation back to c6 recovers the full graph.
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
	c4 := agent.Delete("kb/beta.md", "retract beta")
	c5 := agent.Write("kb/delta.md", testenv.Fact("delta").Confidence(0.7), "init delta")
	c6 := agent.Write("kb/epsilon.md",
		testenv.Fact("epsilon").Refs("kb/gamma.md", "kb/delta.md"),
		"epsilon refs gamma and delta")

	// ── retract delta ─────────────────────────────────────────────────
	c7 := agent.Delete("kb/delta.md", "retract delta")

	// ── subsume gamma+epsilon into zeta ──────────────────────────────
	c8 := agent.Write("kb/zeta.md",
		testenv.Fact("zeta").Refs("kb/gamma.md", "kb/epsilon.md"),
		"zeta subsumes gamma+epsilon")
	agent.Delete("kb/gamma.md", "remove subsumed gamma")
	agent.Delete("kb/epsilon.md", "remove subsumed epsilon")

	// ── assertions ───────────────────────────────────────────────────

	// 1. At c1: only alpha exists.
	c1.Fact("kb/alpha.md").MustExist()
	c1.Fact("kb/beta.md").MustNotExist()

	// 2. At c2: alpha and beta both exist.
	c2.Fact("kb/alpha.md").MustExist()
	c2.Fact("kb/beta.md").MustExist()

	// 3. At c3: gamma reaches both roots.
	c3.Fact("kb/gamma.md").FollowRef("kb/alpha.md").MustExist()
	c3.Fact("kb/gamma.md").FollowRef("kb/alpha.md").Confidence().MustEqual(0.9)
	c3.Fact("kb/gamma.md").FollowRef("kb/beta.md").MustExist()
	c3.Fact("kb/gamma.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)

	// 4. At c4: beta retracted — gamma→beta is broken, gamma→alpha still OK.
	c4.Fact("kb/gamma.md").MustExist()
	c4.Fact("kb/gamma.md").FollowRef("kb/alpha.md").MustExist()
	c4.Fact("kb/gamma.md").FollowRef("kb/beta.md").MustBeBroken()

	// 5. At c5: delta added but epsilon doesn't exist yet.
	c5.Fact("kb/delta.md").MustExist()
	c5.Fact("kb/delta.md").Confidence().MustEqual(0.7)
	c5.Fact("kb/epsilon.md").MustNotExist()

	// 6. At c6: the richest moment — full living graph.
	//    epsilon → gamma         ✓
	//    epsilon → delta         ✓
	//    gamma → alpha           ✓  (3-hop chain: epsilon → gamma → alpha)
	//    gamma → beta            ✗  broken (retracted at c4)
	c6.Fact("kb/epsilon.md").FollowRef("kb/gamma.md").MustExist()
	c6.Fact("kb/epsilon.md").FollowRef("kb/delta.md").MustExist()
	c6.Fact("kb/epsilon.md").FollowRef("kb/delta.md").Confidence().MustEqual(0.7)
	// Walk the 3-hop chain: epsilon → gamma → alpha
	c6.Fact("kb/epsilon.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		MustExist()
	c6.Fact("kb/epsilon.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Confidence().MustEqual(0.9)
	// gamma → beta is already broken at c6 (retracted at c4).
	c6.Fact("kb/epsilon.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/beta.md").
		MustBeBroken()

	// 7. At c7: delta retracted — epsilon→delta broken, epsilon→gamma OK.
	c7.Fact("kb/epsilon.md").FollowRef("kb/delta.md").MustBeBroken()
	c7.Fact("kb/epsilon.md").FollowRef("kb/gamma.md").MustExist()
	c7.Fact("kb/epsilon.md").FollowRef("kb/gamma.md").FollowRef("kb/alpha.md").MustExist()

	// 8. At c8: zeta written, then gamma deleted. zeta→gamma broken,
	//    zeta→epsilon still alive (epsilon deleted in the NEXT commit).
	c8.Fact("kb/zeta.md").MustExist()
	c8.Fact("kb/zeta.md").FollowRef("kb/epsilon.md").MustExist()
	// gamma was deleted in the same batch as zeta's creation commit:
	// c8 is the zeta write; gamma delete is the commit AFTER c8.
	// So at c8, gamma still exists.
	c8.Fact("kb/zeta.md").FollowRef("kb/gamma.md").MustExist()
	// Walk deeper: zeta → gamma → alpha (still alive at c8).
	c8.Fact("kb/zeta.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Confidence().MustEqual(0.9)

	// 9. At HEAD: only alpha and zeta survive.
	head := agent.Head()
	head.Fact("kb/alpha.md").MustExist()
	head.Fact("kb/beta.md").MustNotExist()
	head.Fact("kb/gamma.md").MustNotExist()
	head.Fact("kb/delta.md").MustNotExist()
	head.Fact("kb/epsilon.md").MustNotExist()
	head.Fact("kb/zeta.md").MustExist()
	// zeta's refs both point to deleted facts at HEAD.
	head.Fact("kb/zeta.md").FollowRef("kb/gamma.md").MustBeBroken()
	head.Fact("kb/zeta.md").FollowRef("kb/epsilon.md").MustBeBroken()

	// 10. Temporal recovery: go back to c6 where the deepest chain was
	//     alive. Walk the full path from the bottom up:
	//     epsilon → gamma → alpha  (confidence 0.9)
	//     epsilon → delta          (confidence 0.7)
	//     This is the payoff: HEAD is full of broken links, but temporal
	//     navigation back to c6 recovers the full graph.
	c6.Fact("kb/epsilon.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Confidence().MustEqual(0.9)
	c6.Fact("kb/epsilon.md").
		FollowRef("kb/delta.md").
		Confidence().MustEqual(0.7)

	// 11. And from c3 (before any retractions), gamma still reaches beta.
	c3.Fact("kb/gamma.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)

	// 12. Full walk from zeta at c8 — every node reachable through
	//     historical navigation even though at HEAD only alpha and zeta
	//     survive. At c8, gamma and epsilon are still alive (deleted in
	//     later commits), delta is gone (retracted at c7), beta is gone
	//     (retracted at c4).
	//
	//     zeta → gamma (alive at c8)
	c8.Fact("kb/zeta.md").FollowRef("kb/gamma.md").MustExist()
	//     zeta → gamma → alpha (alive at c8, confidence 0.9)
	c8.Fact("kb/zeta.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Confidence().MustEqual(0.9)
	//     zeta → gamma → beta (broken — retracted at c4)
	c8.Fact("kb/zeta.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/beta.md").
		MustBeBroken()
	//     zeta → epsilon (alive at c8)
	c8.Fact("kb/zeta.md").FollowRef("kb/epsilon.md").MustExist()
	//     zeta → epsilon → gamma → alpha (4 hops, all alive at c8)
	c8.Fact("kb/zeta.md").
		FollowRef("kb/epsilon.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Confidence().MustEqual(0.9)
	//     zeta → epsilon → delta (broken — retracted at c7)
	c8.Fact("kb/zeta.md").
		FollowRef("kb/epsilon.md").
		FollowRef("kb/delta.md").
		MustBeBroken()

	// 13. To reach beta from zeta we need a time machine: go back to
	//     c3 where gamma→beta was alive, then walk that link.
	//     This proves every node in the graph (alpha, beta, gamma,
	//     delta, epsilon, zeta) is reachable via temporal navigation
	//     even though four of six are deleted at HEAD.
	c3.Fact("kb/gamma.md").
		FollowRef("kb/beta.md").
		Confidence().MustEqual(0.8)
	c3.Fact("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Confidence().MustEqual(0.9)
}
