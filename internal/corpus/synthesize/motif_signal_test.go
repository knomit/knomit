package synthesize

import (
	"testing"

	"knomit/internal/corpus/synthesize/step"

	"github.com/stretchr/testify/require"
)

// ── §6: the distill enrichment line ───────────────────────────────────────

// SHARED, not merely present: a motif on one member says something about that
// member; a motif on several says something about the group, which is what the
// synthesized claim is about.
func TestDistillEnrichment_ListsOnlySharedMotifs(t *testing.T) {
	got := sharedClusterMotifs([]step.FactForLLM{
		{File: "a", Motifs: []string{"silent-fallback", "only-mine-here"}},
		{File: "b", Motifs: []string{"silent-fallback"}},
		{File: "c", Motifs: []string{"config-drift"}},
	})
	require.Equal(t, "silent-fallback", got)
	require.NotContains(t, got, "only-mine-here", "one carrier is not a shared motif")
	require.NotContains(t, got, "config-drift")
}

// One fact is one carrier however often it repeats itself, or a single fact
// listing a motif twice would look like a shared one.
func TestDistillEnrichment_OneFactIsOneCarrier(t *testing.T) {
	require.Empty(t, sharedClusterMotifs([]step.FactForLLM{
		{File: "a", Motifs: []string{"silent-fallback", "silent-fallback"}},
	}))
}

// A cluster sharing nothing gets no line at all — not an empty one.
func TestDistillEnrichment_NoSharedMotifsMeansNoLine(t *testing.T) {
	require.Empty(t, sharedClusterMotifs([]step.FactForLLM{
		{File: "a", Motifs: []string{"silent-fallback"}},
		{File: "b", Motifs: []string{"config-drift"}},
	}))

	content, err := RenderDistillWorkItem([]step.FactForLLM{{File: "a"}}, "kb", "", false)
	require.NoError(t, err)
	require.NotContains(t, content.Prompt, "Motifs already shared",
		"a motif-free cluster must see no motif line — free context that says "+
			"nothing is not free")
}

func TestDistillEnrichment_SharedMotifsReachThePrompt(t *testing.T) {
	content, err := RenderDistillWorkItem([]step.FactForLLM{
		{File: "a", Motifs: []string{"silent-fallback"}},
		{File: "b", Motifs: []string{"silent-fallback"}},
	}, "kb", "", false)
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "Motifs already shared by several of these facts")
	require.Contains(t, content.Prompt, "silent-fallback")
}
