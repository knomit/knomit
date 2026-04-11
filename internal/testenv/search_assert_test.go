package testenv

import (
	"testing"
)

// TestSearch_EmptyQueryListsAllFacts asserts that a no-text search
// returns every fact on the branch — the simplest path through the
// production Search API and the one most tests rely on.
//
// Full-text/vector search relies on embedding similarity, which the
// DeterministicEmbedder stub does not actually preserve (it is a
// hash-stretch, not a semantic embedder). Tests that need text-ranked
// results should use the production ONNX embedder or pin exact text.
// For the DSL's SearchAssert smoke tests, the empty-query path is the
// right exercise.
func TestSearch_EmptyQueryListsAllFacts(t *testing.T) {
	t.Log("Scenario: write two facts, empty-text search returns both")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")
	agent.Write("kb/qe.md", Fact("quantum entanglement").Body("a phenomenon where particles share state"), "add qe")
	agent.Write("kb/other.md", Fact("unrelated").Body("nothing in common"), "add unrelated")

	agent.Search("").MustReturn("kb/qe.md", "kb/other.md")
}

// TestSearch_DeleteRemovesFromIndex asserts that after a fact is deleted,
// the empty-text search no longer returns it.
func TestSearch_DeleteRemovesFromIndex(t *testing.T) {
	t.Log("Scenario: write two facts, delete one, empty-text search returns only the surviving one")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")
	agent.Write("kb/x.md", Fact("retractable fact").Body("will be deleted"), "add x")
	agent.Write("kb/y.md", Fact("keeper").Body("will stay"), "add y")
	agent.Delete("kb/x.md", "retract x")

	agent.Search("").MustReturn("kb/y.md").MustNotReturn("kb/x.md")
}

// TestSearch_MustHaveLenAndRankFirst asserts that MustHaveLen and
// MustRankFirst work against the production empty-text results (which
// have uniform score 100, so rank-first is well-defined but not
// particularly meaningful — we just assert the helpers plumb correctly).
func TestSearch_MustHaveLenAndRankFirst(t *testing.T) {
	t.Log("Scenario: three facts, Search().MustHaveLen(3).MustRankFirst(existing-path) succeeds")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")
	agent.Write("kb/a.md", Fact("a").Body("alpha"), "a")
	agent.Write("kb/b.md", Fact("b").Body("beta"), "b")
	agent.Write("kb/c.md", Fact("c").Body("gamma"), "c")

	// Must have all three, and rank-first must be one of them — we
	// don't know which because the production text-less path's ordering
	// is implementation-defined. Just grab whatever the top hit is and
	// confirm it is one of the three.
	sa := agent.Search("")
	sa.MustHaveLen(3)
	top := sa.Results()[0].Path
	sa.MustRankFirst(top)
}

// TestSearch_BranchIsolation asserts that searches are scoped to the
// specified branch — a fact on feature does not appear in a search on main.
func TestSearch_BranchIsolation(t *testing.T) {
	t.Log("Scenario: fact on feature does not show up in search on main")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	feature := repo.BranchFrom("feature", "main")
	feature.Write("kb/secret.md", Fact("secret").Body("only on feature"), "add secret")

	// main does NOT contain kb/secret.md.
	main.Search("").MustNotReturn("kb/secret.md")
	// feature DOES.
	feature.Search("").MustReturn("kb/secret.md")
}
