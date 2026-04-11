// Category C addendum — ref resolution returns the fact state at the
// moment the reference was defined, not at HEAD.
//
// This is the core temporal invariant: when fact X at commit Cn
// references fact Y, following that ref must return Y as it existed
// at Cn — even if Y has been updated since.
package storytests

import (
	"testing"

	"knomit/internal/testenv"
)

// TestRefTemporal_StateAtDefinitionTime builds a chain of facts with
// interleaved updates and verifies that FollowRef always returns the
// state of the target as it was when the referencing commit was made.
//
//	c1: alpha  (confidence 0.8, body "init alpha")
//	c2: beta   (confidence 0.8, body "init beta")
//	c3: gamma → [alpha, beta]  (body "init gamma")
//	c4: update beta  (confidence 0.9, body "beta update 1")
//	c5: update alpha (confidence 0.9, body "alpha update 1")
//	c6: delta → [gamma]
//	c7: update gamma (body "gamma update 1")
//
// At c3, gamma was written when alpha="init alpha" and beta="init beta".
// Following gamma→beta at c3 must return beta's c2 state, not c4.
//
// At c6, delta was written when gamma="init gamma", beta="beta update 1",
// alpha="alpha update 1". Following delta→gamma→beta at c6 must return
// beta at c6 (which is "beta update 1"), and gamma at c6 (which is
// "init gamma" — the c7 update hasn't happened yet).
func TestRefTemporal_StateAtDefinitionTime(t *testing.T) {
	t.Log("C9: FollowRef returns the target's state at the moment the ref was defined, not HEAD")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	// ── mutations ────────────────────────────────────────────────────
	agent.Write("kb/alpha.md",
		testenv.Fact("alpha").Confidence(0.8).Body("init alpha"),
		"init alpha")
	agent.Write("kb/beta.md",
		testenv.Fact("beta").Confidence(0.8).Body("init beta"),
		"init beta")
	c3 := agent.Write("kb/gamma.md",
		testenv.Fact("gamma").Refs("kb/alpha.md", "kb/beta.md").Body("init gamma"),
		"init gamma refs alpha+beta")
	agent.Update("kb/beta.md",
		testenv.Fact("beta").Confidence(0.9).Body("beta update 1"),
		"update beta")
	agent.Update("kb/alpha.md",
		testenv.Fact("alpha").Confidence(0.9).Body("alpha update 1"),
		"update alpha")
	c6 := agent.Write("kb/delta.md",
		testenv.Fact("delta").Refs("kb/gamma.md").Body("init delta"),
		"delta refs gamma")
	agent.Update("kb/gamma.md",
		testenv.Fact("gamma").Refs("kb/alpha.md", "kb/beta.md").Body("gamma update 1"),
		"update gamma")

	// ── assertions ───────────────────────────────────────────────────

	// 1. At c3 (gamma written), following gamma→beta must see beta's
	//    ORIGINAL state (confidence 0.8, body "init beta"), NOT the
	//    later update (confidence 0.9, body "beta update 1").
	c3.Fact("kb/gamma.md").FollowRef("kb/beta.md").Confidence().MustEqual(0.8)
	c3.Fact("kb/gamma.md").FollowRef("kb/beta.md").Body().MustContain("init beta")

	// gamma→alpha at c3: alpha is still at its initial state.
	c3.Fact("kb/gamma.md").FollowRef("kb/alpha.md").Confidence().MustEqual(0.8)
	c3.Fact("kb/gamma.md").FollowRef("kb/alpha.md").Body().MustContain("init alpha")

	// gamma itself at c3 has the original body.
	c3.Fact("kb/gamma.md").Body().MustContain("init gamma")

	// 2. At c6 (delta written), walk delta → gamma.
	//    gamma has NOT been updated yet (c7 is later), so body is "init gamma".
	c6.Fact("kb/delta.md").FollowRef("kb/gamma.md").Body().MustContain("init gamma")

	// 3. Walk delta → gamma → beta at c6.
	//    Beta WAS updated at c4 (before c6), so at c6's snapshot beta
	//    has confidence 0.9 and body "beta update 1".
	c6.Fact("kb/delta.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/beta.md").
		Confidence().MustEqual(0.9)
	c6.Fact("kb/delta.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/beta.md").
		Body().MustContain("beta update 1")

	// 4. Walk delta → gamma → alpha at c6.
	//    Alpha was updated at c5 (before c6), so at c6 alpha has
	//    confidence 0.9 and body "alpha update 1".
	c6.Fact("kb/delta.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Confidence().MustEqual(0.9)
	c6.Fact("kb/delta.md").
		FollowRef("kb/gamma.md").
		FollowRef("kb/alpha.md").
		Body().MustContain("alpha update 1")

	// 5. At HEAD, gamma has been updated to "gamma update 1".
	//    Walking delta→gamma at HEAD returns the UPDATED gamma.
	head := agent.Head()
	head.Fact("kb/delta.md").FollowRef("kb/gamma.md").Body().MustContain("gamma update 1")

	// But from c6, gamma is still "init gamma" — temporal invariant holds.
	c6.Fact("kb/delta.md").FollowRef("kb/gamma.md").Body().MustContain("init gamma")
}
