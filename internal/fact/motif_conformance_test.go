package fact

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// repoRoot walks up from the test's working directory to the module root.
// It FAILS rather than skips when it cannot find one: every check in this file
// is a constraint the roadmap enforces by inspection, and a conformance test
// that cannot locate its subject must say so, not report success.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqualf(t, parent, dir,
			"no go.mod above %s — the conformance scans below cannot run and are NOT being enforced", dir)
		dir = parent
	}
}

// goSources returns every non-test .go file under internal/, keyed by its
// repo-relative path.
func goSources(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	require.NoError(t, filepath.Walk(filepath.Join(root, "internal"),
		func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		}))
	// A scan that examined nothing is not a check.
	require.Greater(t, len(out), 100, "the source scan must actually be reading the tree")
	return out
}

// motifRuleNames are the helpers that define the motif contract. Any use of
// one outside internal/fact is a second place the rules are applied.
var motifRuleNames = regexp.MustCompile(`\b(ValidateMotifs|StripSubjectMotifs|DropInvalidMotifs|MaxMotifs)\b`)

// TestMN4_MotifValidationHasOneCallSite is the roadmap's MN4 grep, as a test.
//
// It fails on any non-test file outside internal/fact that touches the motif
// rule helpers. That is the property MN4 actually protects: the rules can be
// DEFINED once and still be re-applied by a handler that "just checks the
// count first", which is exactly how per-path validation grows back.
func TestMN4_MotifValidationHasOneCallSite(t *testing.T) {
	callers := map[string][]string{}
	for rel, src := range goSources(t) {
		if strings.HasPrefix(rel, "internal/fact/") {
			continue // the definition site
		}
		if hits := motifRuleNames.FindAllString(src, -1); len(hits) > 0 {
			callers[rel] = hits
		}
	}

	// internal/mcp/learn.go references MaxMotifs to TRIM the dedup-merge union.
	// That is blueprint §2.1's mechanical rule, not a second validation site,
	// and it is the sole permitted reference.
	permitted := callers["internal/mcp/learn.go"]
	delete(callers, "internal/mcp/learn.go")
	require.Empty(t, callers,
		"MN4: the motif rules are defined AND applied in internal/fact only")

	// Pin WHAT the exception may name, not how many times it says it: the trim
	// is one rule spelled over two lines (a bound check and a slice), and
	// re-spelling it must not fail this. What must never appear there is a
	// rule HELPER — that would be learn.go deciding for itself what a valid
	// motif is, which is the failure MN4 exists to prevent.
	require.NotEmpty(t, permitted, "the trim in learn.go must still reference MaxMotifs")
	for _, hit := range permitted {
		require.Equalf(t, "MaxMotifs", hit,
			"internal/mcp/learn.go may reference MaxMotifs to trim the merge union, never %s", hit)
	}
}

// TestMN6_MotifsDoNotDriveMechanics — MN6 as clarified by the designer on
// 2026-08-21: the restriction is about MECHANICS, not visibility.
//
// Motifs may be READ by anyone — UI, query/explain output, the ontology rule
// sandbox, serialization. None of those is a "consumer" in this rule's sense,
// and this test deliberately does not police them. What motifs must never do
// is influence the engine's mechanical decisions: dedup thresholds, clustering,
// search ranking, or anything that spawns work outside the §4/§5/§7 synthesis
// paths designed for them. The files below are those decision paths.
func TestMN6_MotifsDoNotDriveMechanics(t *testing.T) {
	sources := goSources(t)
	for _, rel := range []string{
		"internal/synthesize/dedup.go",
		"internal/synthesize/bridge.go",
		"internal/synthesize/bridge_score.go",
		"internal/synthesize/bridge_filtered.go",
		"internal/synthesize/bridge_reshape.go",
		"internal/synthesize/restatement.go",
		"internal/store/search_query.go",
	} {
		src, ok := sources[rel]
		// ERROR, not skip: if one of these was renamed, this test has stopped
		// checking the thing it names and must say so rather than shrink.
		require.Truef(t, ok,
			"MN6 target %s is missing — update this list, do not let the check lapse", rel)
		require.NotContainsf(t, strings.ToLower(src), "motif",
			"MN6: %s is a mechanical decision path and must not read motifs", rel)
	}
}

// TestMN2_NoLLMInMotifCode — vacuous this phase, and stated anyway so the
// phase-3 enumeration loop inherits a check that already exists rather than
// needing one written under deadline.
func TestMN2_NoLLMInMotifCode(t *testing.T) {
	sources := goSources(t)
	for _, rel := range []string{"internal/fact/motif.go", "internal/textnorm/textnorm.go"} {
		src, ok := sources[rel]
		require.Truef(t, ok, "MN2 target %s is missing", rel)
		lower := strings.ToLower(src)
		for _, banned := range []string{"internal/llm", "anthropic", "openai", "completion"} {
			require.NotContainsf(t, lower, banned,
				"MN2: %s must reach no LLM — all motif gating is mechanical", rel)
		}
	}
}

// TestMN13_MotifConstantsAreClassified — every numeric constant in the motif
// path is a documented budget, never a corpus-property constant. Phase 1
// introduces three (MaxMotifs and the two word bounds) and no float literal at
// all; a float appearing here would be a threshold in disguise.
func TestMN13_MotifConstantsAreClassified(t *testing.T) {
	src := goSources(t)["internal/fact/motif.go"]
	require.NotEmpty(t, src)

	require.Regexp(t, `(?s)CONSTANT CLASSIFICATION.*const MaxMotifs`, src,
		"MaxMotifs must state its class where it is defined")
	require.Regexp(t, `(?s)contract, not\s*//?\s*calibration.*minMotifWords`, src,
		"the word bounds must state that they are contract, not calibration")

	floats := regexp.MustCompile(`\b\d+\.\d+\b`).FindAllString(src, -1)
	require.Emptyf(t, floats,
		"MN13: a float literal in the motif path is a corpus-property constant in disguise: %v", floats)
}
