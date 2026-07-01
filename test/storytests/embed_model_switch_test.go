package storytests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/test/testenv"
)

// TestEmbedModelSwitch_TriggersReembed: indexing under one embedding model and
// then restarting under a different one (different ID, same dim) must
// automatically re-embed the corpus — facts_vec is recreated and repopulated,
// and meta records the new model identity. This is the auto-heal-on-model-change
// feature end to end.
func TestEmbedModelSwitch_TriggersReembed(t *testing.T) {
	t.Log("E: switch embedding model → startup heal recreates facts_vec + re-embeds; meta updated")
	sb := testenv.NewStoryboardWithOpts(t, testenv.StoryboardOpts{
		AutoVerify: true, VerifyDeep: true,
		Embedder: &testenv.DeterministicEmbedder{IDOverride: "model-a"},
	})
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	agent.Write("kb/a.md", testenv.Fact("a").Body("first fact body"), "add a")
	agent.Write("kb/b.md", testenv.Fact("b").Body("second fact body"), "add b")

	// Under model-a: both facts have vectors; meta identity is model-a.
	require.Equal(t, 1, factVecCount(t, repo, "kb/a.md"))
	require.Equal(t, "model-a", metaValue(t, repo, "embed_model_id"))

	// Switch to model-b (same dim, different identity) and restart.
	repo.RestartWithEmbedder(&testenv.DeterministicEmbedder{IDOverride: "model-b"})

	// Heal must have re-embedded under model-b: vectors present again, meta updated.
	require.Equal(t, "model-b", metaValue(t, repo, "embed_model_id"),
		"startup should detect the model change and persist the new identity")
	require.Equal(t, 1, factVecCount(t, repo, "kb/a.md"),
		"facts_vec should be repopulated after the model switch")
	require.Equal(t, 1, factVecCount(t, repo, "kb/b.md"))
}

// metaValue reads a meta key from the repo's raw SQL.
func metaValue(t *testing.T, r *testenv.RepoHandle, key string) string {
	t.Helper()
	var v string
	require.NoError(t, r.RawSQL().QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v))
	return v
}
