package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDispatch_ArtifactsAreNotTriggerVisible (F25, T7): a trigger never fires
// for a file under the repo-root artifacts/ folder, on learn, update or
// retract.
//
// The sharpest trigger available: a match must start with its topic, so a
// literal `match: "**"` cannot be written; instead the ontology has a kb TOPIC
// named "artifacts", whose default glob is "artifacts/**". The dispatcher
// matches the path RELATIVE to the ontology root (TrimPrefix(root+"/")), and a
// repo-root artifact path keeps its full name through that TrimPrefix — so if
// the trigger diff ever covered the repo root instead of the ontology root,
// "artifacts/runs/a.md" would match "artifacts/**" and fire. The kb fact
// kb/artifacts/runs/a.md, written in the same runs, is the positive control:
// it fires once per episode.
//
// Sabotage: diff the whole tree in DiffFacts (base "" instead of the ontology
// root, isFactPath bypassed) → the artifact fires → red.
func TestDispatch_ArtifactsAreNotTriggerVisible(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	setOntology(t, ri, "id: trig\nname: Triggers\ntopics:\n  artifacts:\n    description: a kb topic that shares the name\n"+
		"    triggers:\n"+trig("all", "[learn, update, retract]", "", "")+
		"  other:\n    description: other\n")

	waitTriggerHead(t, ri, writeOn(t, ri, trigAgent, "artifacts/runs/a.md"))
	waitTriggerHead(t, ri, writeOn(t, ri, trigAgent, "kb/artifacts/runs/a.md"))
	waitTriggerHead(t, ri, updateOn(t, ri, trigAgent, "artifacts/runs/a.md"))
	waitTriggerHead(t, ri, updateOn(t, ri, trigAgent, "kb/artifacts/runs/a.md"))
	waitTriggerHead(t, ri, deleteOn(t, ri, trigAgent, "artifacts/runs/a.md"))
	waitTriggerHead(t, ri, deleteOn(t, ri, trigAgent, "kb/artifacts/runs/a.md"))

	episodes := map[string][]string{}
	for _, f := range firesOf(t, ri, "all") {
		episodes[f.Path] = append(episodes[f.Path], f.Episode)
	}
	require.Empty(t, episodes["artifacts/runs/a.md"], "an artifact is never trigger-visible")
	for p := range episodes {
		require.NotEqual(t, "artifacts/runs/a.md", p)
	}
	require.ElementsMatch(t, []string{"learn", "update", "retract"}, episodes["kb/artifacts/runs/a.md"],
		"the positive control: the kb fact fires once per episode")
}
