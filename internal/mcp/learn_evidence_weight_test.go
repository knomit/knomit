package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// The reproduction knomit fact f62378e5 asks for by name.
//
// It records the defect as DERIVED BY READING the write path, not observed:
// "a test that learns a high-confidence synthesis fact over an existing
// weighted one and asserts the resulting file still carries evidence_weight
// would confirm or kill it." This is that test.
//
// Three preconditions must all hold, and the fixture asserts each rather than
// arranging them silently:
//   1. the existing fact carries a stored evidence_weight;
//   2. the incoming fact WINS newFactWins (higher confidence);
//   3. the incoming fact OMITS origin — which the learn schema tells agents to
//      do — so the merged Origin is empty and computeEvidenceWeights, which
//      gates on the RAW field, skips the fact instead of restamping it.

const weightedExistingPath = "kb/decisions/weighting/existing.md"

// lenMatched pads body so that len(title+" "+body) equals want. The test
// embedder keys on that length alone, so this is what makes the incoming fact
// dedup-match the existing one — controlled, rather than hoped for.
func lenMatched(t *testing.T, title, body string, want int) string {
	t.Helper()
	base := len(title) + 1 + len(body)
	require.LessOrEqual(t, base, want, "body already longer than the target length")
	return body + strings.Repeat(".", want-base)
}

func weightedFactContent(t *testing.T, title, body string, confidence float64, weight float64) string {
	t.Helper()
	w := ""
	if weight > 0 {
		w = fmt.Sprintf("evidence_weight: %g\n", weight)
	}
	return fmt.Sprintf("---\ntype: synthesis\ndomain: [global]\nconfidence: %g\nsources: 3\n"+
		"entities: [designer]\nrefs: []\n%s---\n# %s\n\n%s\n", confidence, w, title, body)
}

func TestLearnHandler_DedupMergeKeepsStoredEvidenceWeight(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	bg := context.Background()

	const targetLen = 120
	existingTitle := "Weighted synthesis"
	existingBody := lenMatched(t, existingTitle, "the corpus already holds this claim", targetLen)

	_, err := svc.Facts().WriteFact(bg, "agent/test", weightedExistingPath,
		weightedFactContent(t, existingTitle, existingBody, 0.5, 0.8),
		"seed a weighted synthesis fact", "test")
	require.NoError(t, err)

	// PRECONDITION 1, asserted: the stored file really carries a weight.
	stored, err := svc.Facts().ReadFact(bg, "agent/test", weightedExistingPath, nil)
	require.NoError(t, err)
	require.Contains(t, stored.Content, "evidence_weight: 0.8",
		"fixture: the existing fact must carry a stored weight, or this test measures nothing")

	// The incoming fact: same embedding bucket (same length), HIGHER
	// confidence so it wins, and no origin key at all.
	incomingTitle := "Weighted synthesis restated"
	incomingBody := lenMatched(t, incomingTitle, "the same claim, said again", targetLen)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "restate",
		"facts": []any{
			map[string]any{
				"topic":      "decisions",
				"category":   "weighting",
				"title":      incomingTitle,
				"body":       incomingBody,
				"type":       "synthesis",
				"domain":     []any{"global"},
				"confidence": 0.9,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}
	res, err := LearnHandler(emb)(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "learn must succeed: %s", resultText(t, res))

	// PRECONDITION 2, asserted: it actually dedup-merged into the existing
	// fact rather than landing at a fresh path — otherwise nothing was merged
	// and the assertion below would pass for the wrong reason.
	require.Equal(t, weightedExistingPath, mergedFactPath(t, res),
		"fixture: the incoming fact must have merged INTO the existing one")

	merged, err := svc.Facts().ReadFact(bg, "agent/test", weightedExistingPath, nil)
	require.NoError(t, err)
	require.Contains(t, merged.Content, "Weighted synthesis restated",
		"fixture: the incoming fact must have WON the merge")

	require.Contains(t, merged.Content, "evidence_weight:",
		"a dedup-merge must not erase the existing fact's stored evidence_weight — "+
			"the merged fact rests on strictly more evidence, not less")
}
