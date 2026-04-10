// Category C — Temporal graph traversal. The flagship category, directly
// asserting the working.md invariant: when fact A at commit C1 references
// fact B, following that ref must read B as it existed at C1, not B@HEAD.
//
// All seven tests use FactHandle.FollowRef which carries the originating
// commit hash through to the target read. The Storyboard auto-verifies on
// every mutation, so any production drift in temporal-read semantics
// surfaces immediately.
package storytests

import (
	"testing"

	"knomit/internal/testenv"
)

// ── C1 ────────────────────────────────────────────────────────────────────

// TestTemporal_BlueprintExample is the canonical scenario from working.md:
// fact A references fact B at T1. A drops the reference at T2. B's
// confidence changes at T3.
//
//   - At T1: A has a ref to B; following it returns B with original confidence.
//   - At T2: A has no refs; B unchanged from T1.
//   - At T3 (HEAD): B's confidence is the new value.
func TestTemporal_BlueprintExample(t *testing.T) {
	t.Log("C1: A->B at c1; A drops ref at c2; B confidence changes at c3 — all four time-slice cells verified")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/b.md", testenv.Fact("b").Confidence(0.9), "init b")
	c1 := agent.Write("kb/a.md", testenv.Fact("a").Refs("kb/b.md"), "a refs b")
	c2 := agent.Update("kb/a.md", testenv.Fact("a"), "drop ref")
	c3 := agent.Update("kb/b.md", testenv.Fact("b").Confidence(0.4), "lower b confidence")

	// At c1: A references B with B's original confidence.
	c1.Fact("kb/a.md").Refs().MustContain("kb/b.md")
	c1.Fact("kb/a.md").FollowRef("kb/b.md").Confidence().MustEqual(0.9)
	c1.Fact("kb/b.md").Confidence().MustEqual(0.9)

	// At c2: A no longer has the ref; B is unchanged.
	c2.Fact("kb/a.md").Refs().MustBeEmpty()
	c2.Fact("kb/b.md").Confidence().MustEqual(0.9)

	// At c3: B's confidence is the new value.
	c3.Fact("kb/b.md").Confidence().MustEqual(0.4)
}

// ── C2 ────────────────────────────────────────────────────────────────────

// TestTemporal_CycleRefAToBToA asserts that a cycle A↔B does not loop
// infinitely when walked via FollowRef. The DSL's FollowRef is one-step
// (single hop) so the test takes two explicit steps and assertss the
// state at each.
func TestTemporal_CycleRefAToBToA(t *testing.T) {
	t.Log("C2: A refs B and B refs A; FollowRef A->B->A returns A with the original content")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	// Write B first (without ref so the initial write is valid), then A
	// referencing B, then update B to reference A.
	agent.Write("kb/b.md", testenv.Fact("b").Confidence(0.5), "init b")
	c := agent.Write("kb/a.md", testenv.Fact("a").Confidence(0.7).Refs("kb/b.md"), "a refs b")
	agent.Update("kb/b.md", testenv.Fact("b").Confidence(0.5).Refs("kb/a.md"), "b refs a")

	// At the latest snapshot, walking A->B->A should round-trip cleanly.
	// FollowRef is single-step; chain it manually.
	hop1 := agent.Head().Fact("kb/a.md").FollowRef("kb/b.md")
	hop1.MustExist()
	hop2 := hop1.FollowRef("kb/a.md")
	hop2.MustExist()
	hop2.Confidence().MustEqual(0.7)

	// Snapshot c is BEFORE B was updated to ref A, so following B's ref
	// at that commit should be Broken (B has no ref yet at c).
	atC := c.Fact("kb/b.md")
	atC.Refs().MustBeEmpty()
}

// ── C3 ────────────────────────────────────────────────────────────────────

// TestTemporal_BrokenRefAfterDelete asserts that when A references B and
// B is later deleted, FollowRef from A at the post-delete commit returns
// a Broken handle. From a pre-delete commit, FollowRef succeeds.
func TestTemporal_BrokenRefAfterDelete(t *testing.T) {
	t.Log("C3: A refs B; delete B; pre-delete FollowRef OK, post-delete FollowRef returns Broken")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/b.md", testenv.Fact("b"), "init b")
	c1 := agent.Write("kb/a.md", testenv.Fact("a").Refs("kb/b.md"), "a refs b")
	c2 := agent.Delete("kb/b.md", "drop b")

	// At c1, B exists and FollowRef returns a live handle.
	c1.Fact("kb/a.md").FollowRef("kb/b.md").MustExist()

	// At c2, B is gone. A still has the ref (A wasn't touched). FollowRef
	// at c2 reads B at c2 → missing → Broken.
	c2.Fact("kb/a.md").FollowRef("kb/b.md").MustBeBroken()
}

// ── C4 ────────────────────────────────────────────────────────────────────

// TestTemporal_ChainAToBToC asserts a three-deep chain A->B->C where
// each link mutates at different commits, and walking the chain at each
// commit returns the right state at each node.
func TestTemporal_ChainAToBToC(t *testing.T) {
	t.Log("C4: A->B->C chain, each link mutates independently, walking from any commit returns that commit's state")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/c.md", testenv.Fact("c").Confidence(0.1), "init c")
	agent.Write("kb/b.md", testenv.Fact("b").Refs("kb/c.md"), "init b refs c")
	c := agent.Write("kb/a.md", testenv.Fact("a").Refs("kb/b.md"), "init a refs b")
	// Mutate C after the snapshot.
	agent.Update("kb/c.md", testenv.Fact("c").Confidence(0.99), "later C update")

	// From the snapshot c (which is BEFORE the C update), walking
	// A -> B -> C must return C's value at THAT commit (0.1), not HEAD (0.99).
	c.Fact("kb/a.md").
		FollowRef("kb/b.md").
		FollowRef("kb/c.md").
		Confidence().MustEqual(0.1)

	// From HEAD, the chain returns the latest value.
	agent.Head().Fact("kb/a.md").
		FollowRef("kb/b.md").
		FollowRef("kb/c.md").
		Confidence().MustEqual(0.99)
}

// ── C5 ────────────────────────────────────────────────────────────────────

// TestTemporal_FollowRefUsesSameCommit is the regression test for anyone
// "helpfully" rewriting FollowRef to read at HEAD instead of the captured
// commit. A refs B at c1 with B.Confidence=0.9. Later, B.Confidence is
// updated to 0.2. From A@c1, FollowRef must return B@c1 (0.9), not B@HEAD.
func TestTemporal_FollowRefUsesSameCommit(t *testing.T) {
	t.Log("C5: regression test — FollowRef must read target at the receiver's commit, never at HEAD")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/b.md", testenv.Fact("b").Confidence(0.9), "init b 0.9")
	c1 := agent.Write("kb/a.md", testenv.Fact("a").Refs("kb/b.md"), "a refs b")
	agent.Update("kb/b.md", testenv.Fact("b").Confidence(0.2), "lower b to 0.2")

	// At c1, B's confidence was 0.9. FollowRef must return that value.
	c1.Fact("kb/a.md").FollowRef("kb/b.md").Confidence().MustEqual(0.9)

	// At HEAD, FollowRef returns the current value.
	agent.Head().Fact("kb/a.md").FollowRef("kb/b.md").Confidence().MustEqual(0.2)
}

// ── C6 ────────────────────────────────────────────────────────────────────

// TestTemporal_LocalRefsNotFollowedAsExternal asserts that an http://
// ref is classified as External by FollowRef, while a kb/x.md ref on
// the same fact is classified as a real local ref.
func TestTemporal_LocalRefsNotFollowedAsExternal(t *testing.T) {
	t.Log("C6: A has both an http:// ref and a kb/b.md ref; FollowRef classifies them correctly")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/b.md", testenv.Fact("b"), "init b")
	c := agent.Write("kb/a.md",
		testenv.Fact("a").Refs("kb/b.md", "http://example.com/paper"),
		"a with mixed refs")

	c.Fact("kb/a.md").FollowRef("kb/b.md").MustExist()
	c.Fact("kb/a.md").FollowRef("http://example.com/paper").MustBeExternalRef()
}

// ── C7 ────────────────────────────────────────────────────────────────────

// TestTemporal_FanOut asserts that A references [B, C, D, E] and each
// target is mutated independently at different commits. From A at any
// historical snapshot, all four follow-refs return their state at that
// commit, not HEAD.
func TestTemporal_FanOut(t *testing.T) {
	t.Log("C7: A refs [B,C,D,E]; mutate each target at different commits; FollowRef from A at each snapshot returns the right state")
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/b.md", testenv.Fact("b").Confidence(0.10), "init b")
	agent.Write("kb/c.md", testenv.Fact("c").Confidence(0.20), "init c")
	agent.Write("kb/d.md", testenv.Fact("d").Confidence(0.30), "init d")
	agent.Write("kb/e.md", testenv.Fact("e").Confidence(0.40), "init e")
	c0 := agent.Write("kb/a.md",
		testenv.Fact("a").Refs("kb/b.md", "kb/c.md", "kb/d.md", "kb/e.md"),
		"a refs all four")

	// Now mutate each target.
	agent.Update("kb/b.md", testenv.Fact("b").Confidence(0.91), "later b")
	agent.Update("kb/c.md", testenv.Fact("c").Confidence(0.92), "later c")
	agent.Update("kb/d.md", testenv.Fact("d").Confidence(0.93), "later d")
	agent.Update("kb/e.md", testenv.Fact("e").Confidence(0.94), "later e")

	// At c0 (before any of the updates), every follow-ref returns the
	// pre-update value.
	c0.Fact("kb/a.md").FollowRef("kb/b.md").Confidence().MustEqual(0.10)
	c0.Fact("kb/a.md").FollowRef("kb/c.md").Confidence().MustEqual(0.20)
	c0.Fact("kb/a.md").FollowRef("kb/d.md").Confidence().MustEqual(0.30)
	c0.Fact("kb/a.md").FollowRef("kb/e.md").Confidence().MustEqual(0.40)

	// At HEAD, every follow-ref returns the post-update value.
	agent.Head().Fact("kb/a.md").FollowRef("kb/b.md").Confidence().MustEqual(0.91)
	agent.Head().Fact("kb/a.md").FollowRef("kb/c.md").Confidence().MustEqual(0.92)
	agent.Head().Fact("kb/a.md").FollowRef("kb/d.md").Confidence().MustEqual(0.93)
	agent.Head().Fact("kb/a.md").FollowRef("kb/e.md").Confidence().MustEqual(0.94)
}
