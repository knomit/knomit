package testenv

import (
	"testing"
)

// TestFollowRef_TemporalConsistency is the flagship temporal invariant:
// following a local ref from A@c1 must return B as it existed at c1, NOT
// B@HEAD. This is the regression test for anyone "helpfully" rewriting
// FollowRef to read at HEAD instead of the captured commit.
func TestFollowRef_TemporalConsistency(t *testing.T) {
	t.Log("Scenario: A refs B at c1 with B.confidence=0.9; B updated at c2 to 0.2; FollowRef from A@c1 returns 0.9")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/b.md", Fact("b").Confidence(0.9), "add b")
	c1 := agent.Write("kb/a.md", Fact("a").Refs("kb/b.md"), "add a with ref")
	agent.Update("kb/b.md", Fact("b").Confidence(0.2), "lower b")

	// At c1 B's confidence was 0.9. FollowRef must read B@c1, not B@HEAD.
	c1.Fact("kb/a.md").FollowRef("kb/b.md").Confidence().MustEqual(0.9)
}

// TestFollowRef_BrokenAfterDelete asserts FollowRef returns Broken when
// the ref target was deleted in a later commit and we read the target
// at that later commit (where it's gone).
func TestFollowRef_BrokenAfterDelete(t *testing.T) {
	t.Log("Scenario: A refs B; delete B; FollowRef from A@delete returns Broken (ref target missing)")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/b.md", Fact("b"), "add b")
	agent.Write("kb/a.md", Fact("a").Refs("kb/b.md"), "add a refs b")
	delSnap := agent.Delete("kb/b.md", "drop b")

	// A still has the ref because A wasn't updated. Reading A at the
	// delete commit and following the ref should find B gone.
	delSnap.Fact("kb/a.md").FollowRef("kb/b.md").MustBeBroken()
}

// TestFollowRef_ExternalURL asserts FollowRef on an http:// ref returns
// External without attempting a local lookup.
func TestFollowRef_ExternalURL(t *testing.T) {
	t.Log("Scenario: A has an http:// ref; FollowRef returns External")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.Write("kb/a.md",
		Fact("a").Refs("http://example.com/paper"),
		"add a with external ref")

	snap.Fact("kb/a.md").FollowRef("http://example.com/paper").MustBeExternalRef()
}

// TestFollowRef_SchemelessNonMdIsBroken asserts a SCHEMELESS ref is treated as
// a repo-relative fact path — because that is what schemeless means — and so
// reports Broken when nothing is there, rather than External.
//
// This reverses the earlier rule ("no .md suffix means External, the file might
// be a non-fact resource"). A non-fact resource has a scheme: src:// for source,
// file:/// for the filesystem, https:// for the web. Calling a bare "config.yaml"
// External let a typo'd fact path pass as a deliberate external reference.
func TestFollowRef_SchemelessNonMdIsBroken(t *testing.T) {
	t.Log("Scenario: bare config.yaml is a fact path that resolves to nothing → Broken")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.Write("kb/a.md",
		Fact("a").Refs("config.yaml"),
		"add a")

	snap.Fact("kb/a.md").FollowRef("config.yaml").MustBeBroken()
}

// A source citation is External regardless of the cited file's extension —
// including a markdown one, which the old ".md suffix" rule misread as a local
// fact and then failed to find.
func TestFollowRef_SourceRefIsExternalEvenForMarkdown(t *testing.T) {
	t.Log("Scenario: src:// ref to a .md file is External, not Broken")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	const srcRef = "src://knomit/.claude/plans/design.md@ca1c272"
	snap := agent.Write("kb/a.md",
		Fact("a").Refs(srcRef),
		"add a with a markdown source citation")

	snap.Fact("kb/a.md").FollowRef(srcRef).MustBeExternalRef()
}

// TestFollowRef_ChainedLookups asserts that FollowRef can be chained to
// walk a multi-hop ref path at a consistent commit.
func TestFollowRef_ChainedLookups(t *testing.T) {
	t.Log("Scenario: A->B->C chain; walking from A@c reaches C with its c-time content")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/c.md", Fact("c").Confidence(0.4), "add c")
	agent.Write("kb/b.md", Fact("b").Refs("kb/c.md"), "add b refs c")
	snap := agent.Write("kb/a.md", Fact("a").Refs("kb/b.md"), "add a refs b")
	// Mutate C AFTER the snapshot — it should not affect reads at snap.
	agent.Update("kb/c.md", Fact("c").Confidence(0.99), "later c update")

	snap.Fact("kb/a.md").
		FollowRef("kb/b.md").
		FollowRef("kb/c.md").
		Confidence().MustEqual(0.4)
}
