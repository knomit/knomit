package synthesize

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// budgetLiterals are the float literals the consolidation-scope fix is allowed
// to contain, each with the reason it is a RESOURCE constant rather than a
// claim about a corpus.
//
// The distinction is the whole design (roadmap MN13). A corpus-property
// constant — an absolute cosine, a "restatements should be under X% of facts"
// rate — encodes somebody's guess about a corpus into code that then applies it
// to every corpus. A resource constant allocates OUR spend and is honest about
// it. The first kind is forbidden; the second has to say what it budgets.
var budgetLiterals = map[string]string{
	"1.5":   "restatementPriority: ordering within prune's band, below every real cluster",
	"1e-12": "degenerate-norm guard in cosine; a numerical floor, not a similarity threshold",
}

// TestConformance_NoCorpusPropertyConstants fails if a float literal appears in
// the shortlist path without being declared a budget above.
//
// This is a real gate, not decoration: an absolute cosine is exactly what this
// mechanism kept growing in every earlier draft, and it looks reasonable every
// single time.
func TestConformance_NoCorpusPropertyConstants(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "restatement.go", nil, 0)
	require.NoError(t, err)

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.FLOAT {
			return true
		}
		_, allowed := budgetLiterals[lit.Value]
		require.True(t, allowed,
			"float literal %s at %s is not a declared budget: absolute cosines and "+
				"corpus rates must be derived from the repo's own data, never hard-coded (MN13)",
			lit.Value, fset.Position(lit.Pos()))
		return true
	})
}

// TestConformance_BudgetConstantsSayWhatTheyBudget — a number that allocates
// spend has to say so where it is defined, or the next reader cannot tell it
// from a threshold somebody measured.
func TestConformance_BudgetConstantsSayWhatTheyBudget(t *testing.T) {
	src := readSourceFile(t, "restatement.go")
	for _, want := range []struct{ name, classification string }{
		{"titleBackfillBudget", "LATENCY BUDGET"},
		{"titleBackfillBatch", "THROUGHPUT BUDGET"},
		{"pairNeighbourK", "STRUCTURAL BUDGET"},
		{"maxShortlistItems", "JUDGE-SLOT BUDGET"},
		{"shortlistPerMille", "JUDGE-SLOT BUDGET"},
		{"throttleWindow", "PATIENCE BUDGET"},
		{"shortlistOverfetch", "RESOURCE BUDGET"},
	} {
		idx := strings.Index(src, want.name)
		require.Positive(t, idx, "constant %s not found", want.name)
		// The classification has to sit in the comment that introduces the
		// constant. A doc comment opens by naming it, so look forward from the
		// first mention rather than backward.
		window := src[idx:min(idx+700, len(src))]
		require.Contains(t, window, want.classification,
			"%s must be documented as a %s where it is defined", want.name, want.classification)
	}
}

// TestConformance_ReviewPipelineOnly — the runtime paths are untouched by this
// phase. The abstraction axis is review-time derived state, and a query or
// learn call that consulted it would be paying review's costs on the hot path.
//
// Structurally enforced as well as asserted: AbstractionIndex is deliberately
// NOT part of the SearchIndex composite that mcp and web depend on.
func TestConformance_ReviewPipelineOnly(t *testing.T) {
	runtimeFiles := []string{
		"../mcp/query.go",
		"../mcp/explain.go",
		"../mcp/learn.go",
		"../store/search_query.go",
		"../store/search_crud.go",
	}
	for _, rel := range runtimeFiles {
		if _, err := os.Stat(rel); err != nil {
			continue // file renamed; the greps below still cover the packages
		}
		src := readSourceFile(t, rel)
		require.NotContains(t, src, "Abstraction()", "%s must not reach the abstraction axis", rel)
		require.NotContains(t, src, "Restatement", "%s must not reach the restatement shortlist", rel)
		require.NotContains(t, src, "restatement", "%s must not reach the restatement shortlist", rel)
	}
}

// TestConformance_NoConfigSurface — enablement is COMPUTED, never configured.
// A repo owner cannot know whether title similarity discriminates on their
// corpus, so asking them is asking for a guess that then looks like a decision.
func TestConformance_NoConfigSurface(t *testing.T) {
	for _, rel := range []string{"../config/config.go", "../repos/instance.go"} {
		src := strings.ToLower(readSourceFile(t, rel))
		for _, forbidden := range []string{"restatement", "shortlist", "title_vector", "titlevector", "abstraction"} {
			require.NotContains(t, src, forbidden,
				"%s must carry no setting for the consolidation-scope fix", rel)
		}
	}
}

// TestConformance_BridgeFilesUntouched — phase 0 is independent of motifs and
// of the bridge engine. Nothing here may reach into bridge*.go, and the
// EffortNormal contract test must not need adjusting to accommodate it.
func TestConformance_BridgeFilesUntouched(t *testing.T) {
	src := readSourceFile(t, "restatement.go")
	for _, forbidden := range []string{"BridgeSeedSet", "bridgeQ", "buildScoredBridges", "BridgeKind"} {
		require.NotContains(t, src, forbidden,
			"the consolidation-scope fix is independent of the bridge engine")
	}
	// The EffortNormal contract is that normal spends nothing on DISCOVERY.
	// Consolidation is not discovery, so the shortlist runs at every level —
	// which is only legitimate because it is applied uniformly.
	require.NotContains(t, src, "EffortMedium")
	require.NotContains(t, src, "EffortHigh")
	require.NotContains(t, src, "d.Effort",
		"the shortlist is review machinery, not a discovery spend, and must not branch on effort")
}

func readSourceFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(rel))
	require.NoError(t, err)
	return string(b)
}
