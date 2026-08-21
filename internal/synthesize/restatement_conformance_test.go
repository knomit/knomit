package synthesize

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
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
	"0.99":  "p99 of the standing pair distribution — a REPORTED quantile of this repo's own data",
	"0.999": "p99.9 of the standing pair distribution — reported, read by no branch",
}

// phase0Files are every file the consolidation-scope fix owns. The audit covers
// all of them: an absolute cosine is no less a corpus-property constant for
// being in the store layer, and the first version of this test only looked at
// one file.
var phase0Files = []string{
	"restatement.go",
	"../store/abstraction.go",
}

// TestConformance_NoCorpusPropertyConstants fails if a float literal appears in
// the shortlist path without being declared a budget above.
//
// This is a real gate, not decoration: an absolute cosine is exactly what this
// mechanism kept growing in every earlier draft, and it looks reasonable every
// single time.
func TestConformance_NoCorpusPropertyConstants(t *testing.T) {
	for _, rel := range phase0Files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, nil, 0)
		require.NoError(t, err, "parse %s", rel)

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
}

// TestConformance_BudgetConstantsSayWhatTheyBudget — a number that allocates
// spend has to say so where it is defined, or the next reader cannot tell it
// from a threshold somebody measured.
func TestConformance_BudgetConstantsSayWhatTheyBudget(t *testing.T) {
	src := readSourceFile(t, "restatement.go") + readSourceFile(t, "../store/abstraction.go")
	for _, want := range []struct{ name, classification string }{
		{"titleBackfillBudget", "LATENCY BUDGET"},
		{"titleBackfillBatch", "THROUGHPUT BUDGET"},
		{"pairNeighbourK", "STRUCTURAL BUDGET"},
		{"maxShortlistItems", "JUDGE-SLOT BUDGET"},
		{"shortlistPerMille", "JUDGE-SLOT BUDGET"},
		{"throttleWindow", "PATIENCE BUDGET"},
		{"shortlistOverfetch", "RESOURCE BUDGET"},
		{"throttleProbeInterval", "PATIENCE BUDGET"},
		{"titleKNNOverfetch", "STRUCTURAL BUDGET"},
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
	// Whole packages, not a list of filenames. The first version of this test
	// named five files and skipped any that had been renamed, so it would have
	// gone quietly green on the very refactor most likely to break it.
	scanned := 0
	for _, pkg := range []string{"../mcp", "../web", "../store"} {
		files, err := filepath.Glob(filepath.Join(pkg, "*.go"))
		require.NoError(t, err)
		require.NotEmpty(t, files, "package %s has no Go files — the scan is not covering what it claims", pkg)

		for _, path := range files {
			base := filepath.Base(path)
			if strings.HasSuffix(base, "_test.go") {
				continue
			}
			// The axis lives in the store, so its own implementation and the
			// interface that declares it are the two legitimate mentions.
			if pkg == "../store" && (base == "abstraction.go" || base == "interfaces.go" ||
				base == "vec_table.go" || base == "branch.go" || base == "service.go") {
				continue
			}
			// Parsed, not grepped over raw bytes. The property is
			// REACHABILITY, and a violation is written as an identifier, a
			// selector, or a SQL string naming one of the tables — never as a
			// comment. Raw-text matching failed on the difference: a comment
			// explaining why some unrelated table is legitimate ("the same
			// shape as Phase 0's restatement_verdicts") tripped it, so the
			// check forbade DISCUSSING the axis as well as reaching it.
			//
			// String literals are still inspected, so `SELECT ... FROM
			// restatement_pairs` in the store is caught exactly as before —
			// dropping them would trade a false positive for a false negative,
			// which is the worse half of this trade.
			for _, ref := range codeMentions(t, path, "abstraction", "restatement") {
				require.Failf(t, "review-time state reached from a runtime path",
					"%s must not reach %s — it is review-time derived state", path, ref)
			}
			scanned++
		}
	}
	require.Greater(t, scanned, 50, "the reachability scan must actually be reading files")
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

// TestConformance_BridgeFilesUntouched enforces two roadmap constraints that
// only a DIFF can see: no change under internal/synthesize/bridge*.go, and the
// EffortNormal contract test left byte-identical (MN5).
//
// It runs git, because that is what the constraint is about. An earlier version
// of this test replaced the diff with identifier greps over the source — which
// reads like the same check and is not one: greps cannot see an edit to the
// EffortNormal test at all, so MN5 was being enforced by nothing.
func TestConformance_BridgeFilesUntouched(t *testing.T) {
	base := baseRefOrSkip(t)
	// Diff the WORKING TREE against the merge base, not HEAD against it. The
	// first attempt used base...HEAD and let an uncommitted edit to a bridge
	// file pass — which is the state the check most needs to catch, since that
	// is what a developer is looking at when they run the suite.
	mergeBase, err := exec.Command("git", "merge-base", base, "HEAD").Output()
	require.NoError(t, err, "git merge-base %s HEAD", base)
	out, err := exec.Command("git", "diff", "--name-only", strings.TrimSpace(string(mergeBase))).Output()
	require.NoError(t, err, "git diff against %s", base)

	// A conformance check that examines an empty list is not a check. This work
	// touches many files, so an empty diff means the base ref is wrong, not
	// that the constraint holds.
	changed := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			changed++
		}
	}
	require.Positive(t, changed,
		"diff against %s is empty — this check would pass vacuously", base)

	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		require.NotRegexp(t, `^internal/synthesize/bridge.*\.go$`, name,
			"phase 0 is independent of the bridge engine")
		require.NotEqual(t, "internal/synthesize/review_effort_normal_test.go", name,
			"MN5: the EffortNormal contract test stays byte-identical")
	}
}

// baseRefOrSkip resolves the branch this work sits on top of. It SKIPS with a
// loud reason when the ref is genuinely absent (a shallow CI clone, a fork
// without the upstream ref) rather than passing silently — a conformance test
// that cannot run must say so, not report success.
func baseRefOrSkip(t *testing.T) string {
	t.Helper()
	for _, ref := range []string{"dev", "origin/dev", "master", "origin/master"} {
		if err := exec.Command("git", "rev-parse", "--verify", "--quiet", ref).Run(); err == nil {
			return ref
		}
	}
	t.Skip("no base ref (dev/master) available in this checkout: the diff-based " +
		"bridge/MN5 conformance check cannot run here and is NOT being enforced")
	return ""
}

// TestConformance_ShortlistDoesNotBranchOnEffort — consolidation is not
// discovery, so the shortlist runs at every effort level. That is only
// legitimate because it is applied UNIFORMLY, which is what
// invariants/synthesize/effort-normal-byte-identical actually permits.
func TestConformance_ShortlistDoesNotBranchOnEffort(t *testing.T) {
	src := readSourceFile(t, "restatement.go")
	require.NotContains(t, src, "EffortMedium")
	require.NotContains(t, src, "EffortHigh")
	require.NotContains(t, src, "d.Effort",
		"the shortlist is review machinery, not a discovery spend")
	for _, forbidden := range []string{"BridgeSeedSet", "bridgeQ", "buildScoredBridges", "BridgeKind"} {
		require.NotContains(t, src, forbidden,
			"the consolidation-scope fix is independent of the bridge engine")
	}
}

func readSourceFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(rel))
	require.NoError(t, err)
	return string(b)
}

// codeMentions returns the identifiers, selectors and string literals in rel
// whose text contains any of terms (case-insensitively). Comments are NOT
// inspected: the checks above are about what a file REACHES, and prose about a
// subsystem is not a dependency on it.
//
// This is the same shape as internal/fact's funcsMentioningMotifs, and for the
// same reason recorded there: a check has to inspect the form a violation
// would actually be written in. Here that form is code — an identifier, a
// selector, or a SQL string naming a table.
func codeMentions(t *testing.T, rel string, terms ...string) []string {
	t.Helper()
	fset := token.NewFileSet()
	// ParseFile without ParseComments: comments are not part of the AST we walk.
	file, err := parser.ParseFile(fset, filepath.Clean(rel), nil, 0)
	require.NoError(t, err)

	hit := func(s string) string {
		low := strings.ToLower(s)
		for _, term := range terms {
			if strings.Contains(low, term) {
				return s
			}
		}
		return ""
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			if got := hit(v.Name); got != "" {
				out = append(out, got)
			}
		case *ast.SelectorExpr:
			if got := hit(v.Sel.Name); got != "" {
				out = append(out, got)
			}
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if got := hit(v.Value); got != "" {
					out = append(out, got)
				}
			}
		}
		return true
	})
	return out
}
