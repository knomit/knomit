package synthesize

import (
	"crypto/sha256"
	"fmt"
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

// effortNormalTestSHA256 is the SHA-256 of review_effort_normal_test.go, the
// EffortNormal contract test. MN5 says that file stays byte-identical, and
// this constant is how that is enforced.
//
// UPDATING IT IS A DELIBERATE ACT. See the failure message below for what has
// to be true before you do.
//
// THE BYTES ARE STABLE ACROSS PLATFORMS because .gitattributes pins
// `* text=auto eol=lf`, so a Windows checkout gets LF in the working tree like
// everywhere else. That pin exists for exactly this class of problem — see the
// file's own comment, and the autocrlf history in .github/workflows/tests.yml.
// A CRLF checkout WOULD change this hash, and that is the correct outcome
// rather than a bug to paper over: the protected file would genuinely not be
// the bytes this pin names.
const effortNormalTestSHA256 = "bb8f8964828fcfe89f5f24029ae7f92752e4c5d1757a2a8895c56c962919a0e4"

// effortNormalTestFile is the one file MN5 protects.
const effortNormalTestFile = "review_effort_normal_test.go"

// TestConformance_EffortNormalTestByteIdentical enforces MN5: the EffortNormal
// contract test stays byte-identical.
//
// WHY THIS IS A HASH AND NOT A GIT DIFF. It used to be
// TestConformance_BridgeFilesUntouched, which diffed the working tree against
// a dev/master merge base. That check could not run where it mattered and
// could not pass where it ran:
//
//   - On a PUSH TO DEV, actions/checkout makes `dev` point at HEAD, so the
//     merge base IS HEAD, the diff is empty, and the anti-vacuity guard failed
//     the test. 11 of 11 dev runs went red on all three synthesize legs.
//   - On a PULL REQUEST, the depth-1 checkout is detached and carries no
//     dev/master ref at all, so the test SKIPPED and `go test` reported ok.
//
// So it was red exactly where it was meaningless and absent exactly where it
// was meaningful. The guard's premise was wrong: an empty diff means "HEAD is
// the base" as often as it means "wrong base ref". A pinned hash has neither
// failure mode — it runs identically on dev, on a PR, and on a laptop, because
// it asks a question about the FILE rather than about the checkout.
//
// (The phase-0 clause that also lived in the old test — "no
// internal/synthesize/bridge*.go may differ from the merge base" — was removed
// by Phase 3, designer ruling 2026-08-23,
// .claude/plans/motif/2026-08-23-phase3-rulings-1.md Q1: its premise was that
// phase 0 is independent of the bridge engine, and Phase 3 IS the bridge-engine
// phase, so the premise expired rather than the constraint being waived. MN5
// was always the load-bearing half, and it is what survives here.)
func TestConformance_EffortNormalTestByteIdentical(t *testing.T) {
	// ANTI-VACUITY: assert the read, rather than letting a missing file fall
	// through as "not a matching hash". It would fail either way, but only one
	// of those two failures tells you the file is GONE.
	b, err := os.ReadFile(filepath.Clean(effortNormalTestFile))
	require.NoError(t, err,
		"%s could not be read. MN5 protects that file; if it was renamed or "+
			"deleted, this test is the thing that has to be updated deliberately, "+
			"not the thing to delete because it broke.", effortNormalTestFile)
	require.NotEmpty(t, b, "%s is empty", effortNormalTestFile)

	got := fmt.Sprintf("%x", sha256.Sum256(b))
	require.Equal(t, effortNormalTestSHA256, got, `MN5 VIOLATION: %s has changed.

That file is the contract test for effort=normal, and
invariants/synthesize/effort-normal-byte-identical is the invariant it
enforces: at effort=normal the pipeline must spend NOTHING on emergent-fact
discovery — bridgeSeeds returns nil, zero discover work items are enqueued, no
origin=discovered facts are written.

READ THE INVARIANT BEFORE YOU ACT ON THIS. It is NOT a freeze on normal-effort
pipeline behaviour, and it names that misreading explicitly: a change applied
UNIFORMLY at every effort level does not violate it, even one that changes the
work-item queue. What MN5 protects is narrower and is about THIS FILE — that
nobody quietly weakens the test doing the enforcing. Editing the pipeline is
often fine. Editing its enforcing test is the thing that needs an argument.

  observed: %s
  pinned:   %s

If you edited %s on purpose, updating the constant above is part of that
change and your PR has to say why the new contract is still the contract —
which is the review moment this whole mechanism exists to create. If you did
NOT edit it on purpose, you have found an accidental change: revert it.`,
		effortNormalTestFile, got, effortNormalTestSHA256, effortNormalTestFile)
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
