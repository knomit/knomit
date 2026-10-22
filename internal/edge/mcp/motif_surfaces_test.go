package mcp

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"knomit/internal/engine/store/index"
	"knomit/internal/testsupport/modroot"

	"github.com/stretchr/testify/require"
)

// §6's read surfaces. The knob's DEFAULT and its GATING are the parts with
// teeth: loose tiers are for a reader who judges what comes back, and anything
// that triggers automation stays on the §5 operating points.

// The schema's enum is read from the store's tier list, so the parameter
// surface and the implementation cannot drift — the fact-schema single-source
// pattern (invariant 2061717e) applied to this knob.
func TestMotifMatch_SchemaEnumMatchesTheImplementation(t *testing.T) {
	enum := motifMatchEnum()
	require.Len(t, enum, len(index.AllMotifMatchTiers))
	for _, tier := range index.AllMotifMatchTiers {
		require.Contains(t, enum, string(tier))
	}
	require.Equal(t, "exact", enum[0], "the strictest tier is listed first and is the default")
	require.Equal(t, "soft", enum[len(enum)-1], "loosest last")
}

func TestMotifMatch_DefaultsToExact(t *testing.T) {
	tier, err := parseMotifMatch("")
	require.NoError(t, err)
	require.Equal(t, index.MotifMatchExact, tier,
		"an unspecified tier must be the STRICTEST one — a caller who did not choose "+
			"must not silently receive the noisiest results")
}

// An unrecognised tier is an ERROR, not a fall back. Falling back would answer
// a differently-scoped question while looking like it succeeded — and a typo in
// a loose tier would return the strict tier's results, which a caller checking
// for breadth cannot distinguish from "there is nothing looser".
func TestMotifMatch_UnknownTierIsRejectedNotDowngraded(t *testing.T) {
	_, err := parseMotifMatch("fuzzy")
	require.Error(t, err)
	require.Contains(t, err.Error(), "motif_match must be one of")

	_, err = parseMotifMatch("EXACT")
	require.Error(t, err, "tier names are exact strings, not case-insensitive guesses")
}

func TestMotifMatch_EveryTierParses(t *testing.T) {
	for _, tier := range index.AllMotifMatchTiers {
		got, err := parseMotifMatch(string(tier))
		require.NoErrorf(t, err, "tier %q must parse", tier)
		require.Equal(t, tier, got)
	}
}

// A motif-only query is a legitimate query: "what else instantiates this
// mechanism?" is the question the axis exists to answer.
func TestMotifQuery_MotifAloneCountsAsAFilter(t *testing.T) {
	require.True(t, hasAnyFilter(index.SearchOptions{Motifs: []string{"silent-fallback"}}))
	require.False(t, hasAnyFilter(index.SearchOptions{}),
		"...but an empty query is still an empty query")
}

// looseTierRefusers are the functions permitted to NAME a loose tier, with the
// reason each needs to.
//
// The distinction this list encodes is the whole point: a violation is code
// that SELECTS a loose tier, and a function that REFUSES one is the opposite of
// a violation. A raw grep cannot tell them apart — the first version of this
// test failed on motifMatchUnavailable, whose entire job is declining to run
// soft — so the check is per-FUNCTION with declared exceptions, the same shape
// as MN6's, and for the same reason: a check must inspect the form a violation
// would actually be written in.
var looseTierRefusers = map[string]string{
	"motifMatchUnavailable": "refuses soft and explains why; naming the tier is how it declines",
	"parseMotifMatch":       "validates the caller's own value against the tier list; selects nothing",
	"motifMatchEnum":        "renders the tier list for the schema; selects nothing",
	"expandMotifQuery": "IMPLEMENTS the tiers — it dispatches on the tier the CALLER supplied. " +
		"Naming them is how a switch over them is written; what it never does is " +
		"choose one. Found the moment the scan was widened past the MCP package, " +
		"which is the point: the original scan opened only the files least likely " +
		"to offend.",
	"pairSharesCanonicalMotif": "documents in a comment that it uses the EXACT tier only; the " +
		"loose-tier names appear in that reasoning, not in a selection",
}

// TestMotifMatch_LooseTiersAreNeverReachedByAutomation is the §6 rule as a
// test: no code may SELECT token-1 or soft. They exist for a caller who types
// them, and for nobody else — token-1 measured 15 false pairs on the eval set,
// which is acceptable for a human reading results and never for automation.
func TestMotifMatch_LooseTiersAreNeverReachedByAutomation(t *testing.T) {
	// WALKED, not listed. Automation that selected a loose tier would do so in
	// the MCP surface, the synthesis pipeline, or the store's own query path —
	// and a hand-written file list only covers the files someone remembered,
	// which is never the new one. The previous version named eleven files; a
	// twelfth selecting token-1 would have been invisible to it.
	//
	// The package directories ARE the rule's scope, so the scan reads the
	// scope rather than a sample of it. internal/edge/web joined when the REST
	// surface began forwarding motif filters: a handler that defaulted to a
	// loose tier, or an error string that merely NAMED one as available, is
	// exactly what this scan exists to catch — web validation is an allowlist
	// and must stay silent about the tiers beyond it.
	// Every root is WALKED recursively, not listed or read flat: the store's
	// children (internal/engine/store/internal/..., internal/engine/store/fleetrec, ...) and
	// the children of synthesize, web and this package are the same code moved
	// into subpackages, and a flat os.ReadDir or a hand-written directory list
	// silently lets every new child (and any file moved into it) leave the
	// scan while the test stays green.
	nonTest := func(name string) bool {
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}
	var files []string
	// The walk must reach every package directory of each root. The expected
	// set is DERIVED from `go list` (every package of the module under the root
	// that has non-test Go files), never a count: a count is a measured constant
	// that drifts as children are added or removed and under-asserts silently.
	// found is also asserted positive for every root.
	roots := []struct {
		dir string
	}{
		// Resolved from the module root, not by "../sibling": a layer move
		// changes both the depth and the sibling names, and a relative path
		// would break (or, worse, narrow) the scan with it.
		{modroot.Path(t, "internal", "edge", "mcp")},
		{modroot.Path(t, "internal", "corpus", "synthesize")},
		{modroot.Path(t, "internal", "edge", "web")},
		{modroot.Path(t, "internal", "engine", "store")},
		{modroot.Path(t, "internal", "corpus", "repos")},
	}
	for _, root := range roots {
		found := 0
		visited := map[string]bool{}
		require.NoErrorf(t, filepath.WalkDir(root.dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				visited[filepath.Clean(p)] = true
				return nil
			}
			if nonTest(d.Name()) {
				files = append(files, p)
				found++
			}
			return nil
		}), "walk %s", root.dir)
		// Fail closed: a root that suddenly yields nothing is a moved package or
		// a broken relative path, and a scan over zero files reports success just
		// as loudly as a scan that found no violations.
		require.Positivef(t, found, "no Go sources found under %s — the scan is not reading its subject", root.dir)
		rootRel, relErr := filepath.Rel(modroot.Path(t), root.dir)
		require.NoError(t, relErr)
		listed, lerr := exec.Command("go", "list", "-f", "{{if .GoFiles}}{{.Dir}}{{end}}", "knomit/"+filepath.ToSlash(rootRel)+"/...").Output()
		require.NoErrorf(t, lerr, "go list under %s", root.dir)
		expected := strings.Fields(string(listed))
		require.NotEmptyf(t, expected, "go list found no packages under %s: the expected set is empty", root.dir)
		for _, dir := range expected {
			require.Truef(t, visited[filepath.Clean(dir)], "the walk of %s did not visit package directory %s: its children are not being reached", root.dir, dir)
		}
		// Non-vacuous per directory: every directory under the root that has
		// non-test Go files must have contributed them. The walk above and this
		// glob read each directory by different routes, so a walk that stops
		// descending (a skipped directory, a filter on the wrong name) shows up
		// here instead of passing silently.
		require.NoError(t, filepath.WalkDir(root.dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			matches, gerr := filepath.Glob(filepath.Join(p, "*.go"))
			require.NoError(t, gerr)
			for _, m := range matches {
				if nonTest(filepath.Base(m)) {
					require.Containsf(t, files, m, "%s has non-test Go sources the scan did not read", p)
				}
			}
			return nil
		}))
	}

	for _, rel := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Clean(rel), nil, 0)
		require.NoErrorf(t, err, "parse %s", rel)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var named []string
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.Ident:
					if v.Name == "MotifMatchToken1" || v.Name == "MotifMatchSoft" {
						named = append(named, v.Name)
					}
				case *ast.SelectorExpr:
					if v.Sel.Name == "MotifMatchToken1" || v.Sel.Name == "MotifMatchSoft" {
						named = append(named, v.Sel.Name)
					}
				case *ast.BasicLit:
					if v.Kind == token.STRING && (v.Value == `"token-1"` || v.Value == `"soft"`) {
						named = append(named, v.Value)
					}
				}
				return true
			})
			if len(named) == 0 {
				continue
			}
			require.Containsf(t, looseTierRefusers, fn.Name.Name,
				"%s.%s names the loose tier(s) %v. Loose tiers are reachable ONLY by "+
					"explicit caller parameter. If this function REFUSES one rather than "+
					"selecting it, declare it in looseTierRefusers with the reason.",
				rel, fn.Name.Name, named)
		}
	}
	// No "checked == len(files)" assertion here. There was one, and it could not
	// fail: the counter was incremented once per iteration immediately after the
	// parse, so it equalled the length by construction. A check a comment could
	// satisfy. The real coverage guard is the per-directory Positive above,
	// which fails when the scan stops reading its subject.

	// Bidirectional, like the fact package's lists: a declared refuser that no
	// longer exists is a permission nobody needs and nobody notices going stale.
	for name, why := range looseTierRefusers {
		// Matches plain functions AND methods. The first version looked for
		// "func NAME(" only, so a declared METHOD read as missing — which is
		// how the widened scan's first legitimate entry appeared to be stale
		// the moment it was added.
		decl := regexp.MustCompile(`func (\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(`)
		found := false
		for _, rel := range files {
			if decl.MatchString(readMCPSource(t, rel)) {
				found = true
			}
		}
		require.Truef(t, found, "%s is declared a loose-tier refuser (%s) but no longer exists", name, why)
	}
}

func readMCPSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := readFileForTest(rel)
	require.NoError(t, err)
	return string(b)
}

// The explain surface's sibling list is bounded. explain answers about ONE
// fact, and an unbounded list on a popular motif would bury it.
func TestExplainMotifs_SiblingListIsBounded(t *testing.T) {
	require.Greater(t, explainMotifsShown, 0)
	require.LessOrEqual(t, explainMotifsShown, 10,
		"the sibling budget must stay small enough that the fact being explained "+
			"remains the subject of the response")
}

// Motifs are omitted from the wire when absent, so a motif-free corpus's
// responses are byte-identical to what they were before the axis existed.
func TestMotifSurfaces_OmittedWhenAbsent(t *testing.T) {
	fm := frontmatterOutput{Domain: []string{"alpha"}}
	blob, err := marshalForTest(fm)
	require.NoError(t, err)
	require.NotContains(t, string(blob), "motifs")

	fm.Motifs = []string{"silent-fallback"}
	blob, err = marshalForTest(fm)
	require.NoError(t, err)
	require.Contains(t, string(blob), "silent-fallback")

	e := explainFactEntry{Path: "kb/a.md"}
	blob, err = marshalForTest(e)
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(string(blob)), "motifs")
}

func readFileForTest(rel string) ([]byte, error) { return os.ReadFile(filepath.Clean(rel)) }

func marshalForTest(v any) ([]byte, error) { return json.Marshal(v) }
