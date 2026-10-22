package embeddings

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShortStringTemplate_IsTheTitleHack pins the measured rendering for bare
// short strings (fact titles now; motif names and name+definition strings from
// Phase 2 of the motif work).
//
// It looks wrong — a title-slot template with a literal "none" body — and it is
// right: the task sweep recorded in
// .claude/plans/motif/2026-08-20-motif-summarizer-experiment.md measured the
// model card's own task prompts WORSE than this rendering for strings of a few
// words on embeddinggemma. Every operating point calibrated in that work
// assumes this exact string, so changing it invalidates the calibration rather
// than merely perturbing it.
func TestShortStringTemplate_IsTheTitleHack(t *testing.T) {
	m, err := Lookup("embeddinggemma")
	require.NoError(t, err)
	require.Equal(t, "title: {content} | text: none", m.ShortStringTemplate)
	require.NotContains(t, m.ShortStringTemplate, "task:",
		"short strings must never use the model card's task prompt")
}

// TestShortStringTemplate_EveryModelDefinesOne keeps the descriptor total: a
// model with an empty short-string template would silently embed the bare
// string, which is a third rendering nobody measured.
func TestShortStringTemplate_EveryModelDefinesOne(t *testing.T) {
	for _, id := range IDs() {
		m, err := Lookup(id)
		require.NoError(t, err)
		require.NotEmpty(t, m.ShortStringTemplate, "model %s", id)
		require.True(t, strings.Contains(m.ShortStringTemplate, "{content}"),
			"model %s: short-string template must carry a {content} slot", id)
	}
}

// TestShortStringText_RendersThroughTheDescriptor is the guard that every
// short-string embedding site goes through the descriptor rather than
// hand-rolling a template at the call site.
func TestShortStringText_RendersThroughTheDescriptor(t *testing.T) {
	m, err := Lookup("embeddinggemma")
	require.NoError(t, err)
	e := &Embedder{model: m}
	require.Equal(t, "title: prune scope | text: none", e.shortStringText("prune scope"))
}
