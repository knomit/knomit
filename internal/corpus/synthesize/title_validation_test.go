package synthesize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A model-emitted title that is blank or carries a line break would be refused
// later by SerializeFact — for reflect, only after the item was claimed, so the
// whole item (valid reinforcements included) was consumed with nothing
// applied. The validators refuse it up front with the same rule.
func TestValidators_RefuseUnwritableTitles(t *testing.T) {
	for _, title := range []string{"", "   ", "\nFoo", "Foo\nBar", "Foo\r\nBar"} {
		merge := PruneResult{Merges: []MergeEntry{{Paths: []string{"kb/a.md"}, Merged: mergedFact{Title: title}}}}
		require.Error(t, validatePrunePaths(merge, []string{"kb/a.md"}), "prune merge title %q", title)

		propose := ReflectResult{Propose: []ProposeEntry{{
			Title: title, Body: "B", TopicPath: "kb/meta/reasoning", Confidence: 0.7,
			TransitionPaths: []string{"kb/h2.md"}, NoveltyArgument: "n",
		}}}
		err := validateReflectResponse(propose, []string{"kb/h2.md"}, 1)
		require.Error(t, err, "reflect propose title %q", title)
		require.Contains(t, err.Error(), "propose[0]: title")
	}
}
