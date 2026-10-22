package synthesize

import (
	"context"
	"testing"

	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/motif"

	"github.com/stretchr/testify/require"
)

// RenderReflectWorkItem lives in this package, so the tests that follow the
// vocabulary section into the reflect prompt stay here; motif.VocabularySection
// is the exported seam they drive.

// ── §3.3 metrics reaching their surfaces ──────────────────────────────────

// The reflect payload carries recurrence and mint-to-link (§3.3). Reflect is
// where the corpus reasons about its own habits, and "are we naming the same
// mechanisms as each other" is one.
func TestMotifVocabulary_ReachesTheReflectPayload(t *testing.T) {
	ctx := context.Background()
	env := synthtest.NewRestatementEnv(t, 0)
	env.WriteFactWithMotifs("kb/a.md", "A", "body", []string{"silent-fallback"})
	env.WriteFactWithMotifs("kb/b.md", "B", "body", []string{"silent-fallback"})
	env.WriteFactWithMotifs("kb/c.md", "C", "body", []string{"config-drift"})
	require.NoError(t, env.Svc.Motifs().RebuildAliases(ctx, env.Branch))

	section := motif.VocabularySection(ctx, env.Deps(), env.Branch)
	require.Contains(t, section, "motif clusters")
	require.Contains(t, section, "Recurrence")
	require.Contains(t, section, "Mint-to-link")

	content, err := RenderReflectWorkItem([]byte(`[{"path":"kb/hyp/a.md"}]`), "kb", "", section)
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "Recurrence",
		"the reflect prompt must carry the vocabulary metrics when there is a vocabulary")
}

// A corpus with no motif vocabulary gets NO section — not a section of zeroes.
// "0 clusters, recurrence 0%" invites the model to reason about a mechanism the
// corpus is not using.
func TestMotifVocabulary_AbsentSectionOnAMotiflessCorpus(t *testing.T) {
	ctx := context.Background()
	env := synthtest.NewRestatementEnv(t, 3) // plain facts, no motifs
	require.NoError(t, env.Svc.Motifs().RebuildAliases(ctx, env.Branch))

	section := motif.VocabularySection(ctx, env.Deps(), env.Branch)
	require.Empty(t, section)

	content, err := RenderReflectWorkItem([]byte(`[{"path":"kb/hyp/a.md"}]`), "kb", "", section)
	require.NoError(t, err)
	require.NotContains(t, content.Prompt, "Recurrence")
	require.NotContains(t, content.Prompt, "motif clusters",
		"a motif-free corpus must see no motif section at all")
}
