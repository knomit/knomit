package fact

import (
	"go/ast"
	"go/parser"
	"go/token"
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

// motifGateNames are the helpers that DECIDE what a valid motif is. Any use of
// one outside internal/fact is a second place the rules are applied, which is
// exactly what MN4 forbids.
//
// fact.MergeMotifs and fact.MaxMotifs are deliberately NOT here. They are
// writer plumbing, not gate logic: MergeMotifs unions two already-validated
// lists and MaxMotifs is the cap it trims to, and every fact-merging path needs
// them by construction. Policing them as if they were validation is what pushed
// the review-session merge into dropping the loser's motifs to stay "clean".
var motifGateNames = regexp.MustCompile(`\b(ValidateMotifs|StripSubjectMotifs|DropInvalidMotifs)\b`)

// motifGateCallSites are the write paths permitted to invoke the gate helpers
// directly, with the reason each needs to.
//
// Calling the shared gate is not what MN4 forbids — RE-IMPLEMENTING it is. A
// path that asks fact.StripSubjectMotifs what to drop is using the single
// definition, not inventing a second one. The list exists so that a new direct
// caller has to be justified, because needing one usually means the path is not
// reaching SerializeFact, which is the actual defect.
var motifGateCallSites = map[string]string{
	"internal/synthesize/decision.go": "prune-merge and distill consume LLM-PROPOSED motifs on facts whose " +
		"other content is good; DropInvalidMotifs drops what SerializeFact would " +
		"reject so one malformed name cannot discard an entire consolidation. It " +
		"uses the gate's own definition rather than re-implementing one, which is " +
		"what MN4 forbids",
	"internal/synthesize/discovery.go": "same, for discovered facts — which cost a bridge enumeration and an " +
		"LLM call each, so discarding one over a malformed motif spends both for nothing",
	"internal/web/handlers_fact_write.go": "the REST PUT path commits the client's bytes verbatim, so it must " +
		"apply the gate itself and reserialize when the gate changes anything; it is the one write " +
		"path that does not reach SerializeFact on its own",
}

// motifMergeSites are the fact-merging paths permitted to call fact.MergeMotifs.
// Both are the SAME operation in two locations; the list exists so a third one
// cannot appear unnoticed.
var motifMergeSites = map[string]string{
	"internal/mcp/learn.go":        "learn-time dedup merge",
	"internal/synthesize/dedup.go": "review-session consolidation merge",
}

// TestMN4_MotifValidationHasOneCallSite is the roadmap's MN4 grep, as a test.
//
// It fails on any non-test file outside internal/fact that touches the motif
// rule helpers. That is the property MN4 actually protects: the rules can be
// DEFINED once and still be re-applied by a handler that "just checks the
// count first", which is exactly how per-path validation grows back.
func TestMN4_MotifValidationHasOneCallSite(t *testing.T) {
	offenders := map[string][]string{}
	mergeCallers := map[string]bool{}
	for rel, src := range goSources(t) {
		if strings.HasPrefix(rel, "internal/fact/") {
			continue // the definition site
		}
		if hits := motifGateNames.FindAllString(src, -1); len(hits) > 0 {
			offenders[rel] = hits
		}
		if callsFunc(t, rel, "MergeMotifs") {
			mergeCallers[rel] = true
		}
	}

	for rel, hits := range offenders {
		require.Containsf(t, motifGateCallSites, rel,
			"MN4: %s invokes the motif gate %v outside internal/fact. Route the fact "+
				"through SerializeFact instead — or, if this path genuinely commits bytes "+
				"that never reach it, declare it in motifGateCallSites with the reason.", rel, hits)
	}
	for rel, why := range motifGateCallSites {
		require.Containsf(t, offenders, rel,
			"%s is declared a direct gate call site (%s) but no longer calls one — "+
				"remove the entry, or restore the gate it was granted for", rel, why)
	}

	// The merge helper is permitted, but only where a fact merge actually
	// happens, and every declared site must still be one.
	for rel := range mergeCallers {
		require.Containsf(t, motifMergeSites, rel,
			"%s calls fact.MergeMotifs but is not a declared fact-merge site — "+
				"add it to motifMergeSites with a reason, or stop merging motifs there", rel)
	}
	for rel, why := range motifMergeSites {
		require.Truef(t, mergeCallers[rel],
			"%s is declared a motif-merge site (%s) but no longer calls fact.MergeMotifs — "+
				"a merge that drops the loser's motifs silently loses authored data", rel, why)
	}
}

// mechanicsPaths are the engine's mechanical decision paths, mapped to the
// functions within each that may legitimately touch motifs.
//
// Empty slice = no function in that file may mention them at all.
var mechanicsPaths = map[string][]string{
	// dedupCluster is a WRITER: it merges two facts and commits the survivor,
	// so it must carry the loser's motifs across or that authored data is
	// deleted with the loser and no derived state can rebuild it. Every OTHER
	// function here — the similarity scoring, the threshold comparison, the
	// greedy pair selection — must stay blind to motifs, which is the actual
	// MN6 constraint.
	"internal/synthesize/dedup.go":           {"dedupCluster"},
	"internal/synthesize/bridge.go":          nil,
	"internal/synthesize/bridge_score.go":    nil,
	"internal/synthesize/bridge_filtered.go": nil,
	"internal/synthesize/bridge_reshape.go":  nil,
	// The §7 shortlist is a path MN6 names EXPLICITLY as designed for motifs
	// ("anything that spawns work outside the §4/§5/§7 synthesis paths designed
	// for them"), so this is the rule's exemption rather than an exception to
	// it. Blueprint §6 puts a shared exact motif into §7 as an added
	// restatement signal.
	//
	// Scoped to the two functions that implement the widener. Everything else
	// in this file — the pair cache, the percentile ranking, the judge-outcome
	// throttle, the probe — stays blind to motifs, and the widener changes
	// ELIGIBILITY only: the judge-slot budget is untouched, so a motif-rich
	// corpus gets better candidates for the same spend, never more of them.
	//
	// What this does NOT license is a motif term entering the SCORING. A shared
	// motif buys a look further down this repo's own ranking; it does not
	// change where a pair sits in that ranking, which would be a corpus-property
	// claim in disguise (MN13, Q2 ruling).
	"internal/synthesize/restatement.go": {
		"selectRestatementCandidates", "pairSharesCanonicalMotif",
		// REPORTING, not deciding. These render the signal's contribution into
		// the session's health lines so the GATE package can state how often it
		// fired rather than that it exists. No branch reads them — which is the
		// MN6 property, and is itself asserted by the no-branch grep.
		"healthLines", "motifSignalLine",
	},
	// ScopedCluster is a CARRIER, and only the projection inside it (Phase 2).
	// It builds the factForLLM payload from search results, and §2.1 requires
	// that payload to show members' motifs — without it prune and distill
	// cannot carry a motif over to the fact that replaces them, and the merged
	// claim silently loses the regularity its members named.
	//
	// It does not CLUSTER on motifs. The community detection, the similarity
	// scoring, and the resolution parameter are all untouched and stay banned,
	// as does every other function in this file. The entry licenses copying the
	// field into a payload, not consulting it in a decision — if a motif term
	// ever reaches the clustering arithmetic that is an MN6 violation this does
	// not cover.
	// The §4/§5 path itself — the axis MN6 names as DESIGNED for motifs
	// ("anything that spawns work outside the §4/§5/§7 synthesis paths designed
	// for them"). Listed rather than left unpoliced, so this map stays the one
	// register of which functions read motifs and a future helper here has to
	// be declared rather than appearing quietly.
	//
	// What no entry here licenses: a motif term reaching dedup, clustering or
	// search ranking. Those files stay nil below and this list does not touch
	// them.
	// laneOf, scoreMotifCandidate, disjointMembers and copyMembers are
	// deliberately NOT listed: their bodies name no motif identifier (they work
	// on canonical ids, paths and scores), and this list rejects a permission
	// nothing uses.
	//
	// rankAndCap WAS in that sentence until #125 and no longer is. Nothing
	// about what it does changed — it ranked motif bridge candidates before and
	// ranks them now — but its inline sort literal became a call to the shared
	// motifRankLess comparator, which names a motif identifier where the
	// literal named none. That is the check working as designed rather than a
	// false positive: MN6's register is of which functions READ motifs, and
	// this one always did through the values it sorts. The alternative — naming
	// the comparator so the word does not appear — is the fail-OPEN this test's
	// own BasicLit case was added to close, so it was not taken.
	//
	// The names in this paragraph are prose, and the bidirectional machinery
	// checks the allow-LIST rather than the reasoning beside it — so a rename
	// leaves a ghost here that nothing catches. One did: the Phase-3 review
	// (L3) found this sentence still citing mergeToken2Groups, deleted two
	// commits earlier and replaced by token2Families.
	//
	// Of the two listed, enumerateMotifCandidates reads members' motifs to
	// GROUP them and sharedMotifSpecificity reads them to SCORE the group it
	// was already given — both inside the §4/§5 cascade, neither reachable from
	// dedup, clustering or search ranking.
	// buildMotifBridges is the §4/§5 entry point: it reads the seed pool's
	// motifs to decide whether the axis does anything at all, then drives
	// enumeration, the lane split and the per-lane budgets.
	// The wiring half of the same file: motifResolverFor closes over the alias
	// table, and motifBridgeHealthLines RENDERS what the axis did into the
	// session's health output. Reporting is not deciding — the no-branch
	// property is what MN6 is about, and it holds: nothing in this package
	// reads a health line.
	// #125: the rank order gained an entity-overlap TIEBREAKER, and both rank
	// paths now share one comparator (motifRankLess) so the served order and the
	// measured order cannot drift. The two CALLERS name a motif identifier and
	// are listed; motifRankLess itself is not, and deliberately so — its body
	// reads only Q, the overlap and the token, so it mentions nothing, and the
	// bidirectional half of this check refuses a permission nothing uses.
	//
	// What this licenses is ORDERING WITHIN THE AXIS, which is the §4/§5 path
	// MN6 designs motifs into. What it does not license is the inverse — an
	// entity term deciding whether a candidate EXISTS. The tiebreaker sits
	// strictly below Q for exactly that reason, and is a sort term rather than
	// a weight so no constant has to be invented to mean "small" (MN13).
	"internal/synthesize/bridge_motif.go": {
		"buildMotifBridges", "enumerateMotifCandidates", "sharedMotifSpecificity", "anyMotifs",
		"motifResolverFor", "motifBridgeHealthLines", "verbatimGroups", "token2Families",
		"rankAndCap", "rankAndCapRows",
		// #125's qualify predicate. It is DECLARED here rather than beside
		// BridgeKindFromString in bridge.go precisely so that file's nil entry
		// above stays nil: a method whose body names BridgeMotif would have
		// forced the entity/domain engine's "blind to motifs" declaration into
		// a one-entry allow-list, to hold a predicate whose whole subject is
		// the other axis. Moving the method preserved the stronger statement.
		"Qualifies",
		// Phase 4: buildMotifBridges' own loop, extracted so the measurement
		// instrument and production drive the SAME enumeration and scoring
		// rather than two implementations that agree until they do not. It IS
		// the §4/§5 path MN6 designs motifs into — the same licence
		// buildMotifBridges has always had, now naming the function that does
		// the work instead of the wrapper that calls it.
		"scoreMotifCandidates",
		// The drop-cause taxonomy and its tally. They read the SCORER's
		// components (cohesion, member count) and the lane, never a motif's
		// content; they name motifs only in describing what was dropped.
		// Declared rather than exempted, because the register's value is that
		// a motif-touching function cannot appear here undeclared.
		"motifDropCauseOf", "tallyMotifDrops",
		// Phase-4 rulings-3: suppression became TIER-AWARE, so it now reads the
		// motif tier a candidate came from (enumeratedMotif.family) to decide
		// which of two nested groups is served. It reads the tier, never a
		// motif's content, and it is inside the §4/§5 path.
		// suppressContainedTracked replaces suppressContained as the function
		// that reads the tier (M-4 remediation); suppressContained is now a
		// two-line wrapper that mentions no motif, so it comes OFF the list —
		// a permission nothing uses is what the bidirectional check exists to
		// catch.
		"suppressContainedTracked",
		// The single-pass builder and the row-writing rank/cap it delegates
		// to. Same §4/§5 licence buildMotifBridges has always had; theyname the
		// functions that now do the work.
		"buildMotifBridgesWithRows",
		// dedupThresholdFor now names motifs in its own doc (M-5: the
		// MotifDedupThreshold override). It resolves a model's cosine
		// threshold and reads no motif content.
		"dedupThresholdFor",
		// Phase 4: the activation floor. seedRecurrence counts the corpus's
		// recurring motifs and motifActive decides whether the axis runs at
		// all — both read motifs to answer "is there enough repeated
		// vocabulary here", never to influence a dedup, cluster or ranking
		// decision. This IS the §4/§5 path's own enablement.
		"seedRecurrence", "motifActive",
	},
	// Phase 4's measurement entry point (Q3's sibling report). It is a
	// read-only calibrate/dev path: it enumerates and scores through the
	// shipped §4/§5 functions and returns rows. It decides nothing, writes
	// nothing, and no production branch reads it — the same standing as
	// BridgeComponentReport on the entity/domain axis. Listed so a function
	// added to this file has to be declared rather than inheriting the file's
	// silence.
	// Summary renders the report as one human line and names the
	// seeds-with-motifs count in it.
	// token2PairsOf reports which canonical ids the token-2 tier WOULD fold,
	// using the tier's own predicate. It decides nothing — the report is
	// read-only and no branch consults it.
	"internal/synthesize/bridge_motif_report.go": {"MotifComponentReport", "Summary", "token2PairsOf",
		// The row-to-report projection (M-8). It moves a candidate's own
		// disposition into the reported shape and decides nothing.
		"scoredMotifBridgeOf"},
	// The gate that decides which motif groups the agent ever sees. It reads
	// the SUBJECT axis — entities, domain tags, path tokens — to do it, and
	// names motifs only in describing what it gates. Listed with no permitted
	// function so that a motif-reading function added here must be declared.
	"internal/synthesize/motif_disjoint.go": {},
	"internal/synthesize/cluster.go":        {"ScopedCluster"},
	"internal/synthesize/louvain.go":        nil,

	// The read CARRIERS only (Phase 2, designer ruling 2026-08-21). Each of
	// these names a SELECT column list and hands the row to a scanner; carrying
	// an authored field to a reader is the visibility side of the MN6 line,
	// which the clarified rule does not restrict. Phase 1 left the field
	// write-only precisely because no such entry existed, and every read
	// surface would have shipped empty with a green suite.
	//
	// What stays banned in this file is the deciding: newFactFilter's
	// WHERE-clause construction, filterByEpisodeOps, and the KNN/rerank scoring
	// inside Search must not consult motifs. Search appears here because its
	// text-less branch owns a column list, NOT because its ranking may read
	// them — if a motif term ever reaches the scoring arithmetic, that is an
	// MN6 violation this entry does not license.
	//
	// newFactFilter and addMotifClause are the §6 motif_match filter, added in
	// Phase 2 with the designer's Q1 scoping sentence: EXPLICIT user-supplied
	// motif_match ONLY; never consulted unless the caller passed the parameter.
	// A user-requested filter is expressed intent — the visibility side of the
	// MN6 line — and §6 governs read surfaces by design.
	//
	// expandMotifQuery resolves the caller's terms to concrete spellings per
	// tier. Same licence: it answers the question asked, it does not decide
	// what ranks.
	//
	// None of these entries licenses motifs reaching the SCORING or KNN paths.
	// Those decide ranking rather than answering a caller's question, and a
	// motif term arriving there is an MN6 violation this list does not cover.
	"internal/store/search_query.go": {
		"RecentFacts", "recentFactsSearch", "Search",
		"newFactFilter", "addMotifClause", "expandMotifQuery",
	},
}

// TestMN6_MotifsDoNotDriveMechanics — MN6 as clarified by the designer on
// 2026-08-21: the restriction is about MECHANICS, not visibility.
//
// Motifs may be READ by anyone — UI, query/explain output, the ontology rule
// sandbox, serialization — and they may be MERGED by anything that writes
// facts. None of that is a "consumer" in this rule's sense. What motifs must
// never do is influence the engine's mechanical decisions: dedup thresholds,
// clustering, search ranking, or anything that spawns work outside the
// §4/§5/§7 synthesis paths designed for them.
//
// The check is per-FUNCTION rather than per-file, and that distinction is the
// whole point. A file-level ban read MN6 as "dedup.go must not say motif",
// which is how the review-session merge came to drop the loser's motifs: the
// conformance test was enforcing a rule stricter than the one written down,
// and the code obediently lost data to satisfy it.
func TestMN6_MotifsDoNotDriveMechanics(t *testing.T) {
	sources := goSources(t)
	for rel, allowed := range mechanicsPaths {
		// ERROR, not skip: if one of these was renamed, this test has stopped
		// checking the thing it names and must say so rather than shrink.
		require.Containsf(t, sources, rel,
			"MN6 target %s is missing — update this list, do not let the check lapse", rel)

		offenders := funcsMentioningMotifs(t, rel)
		for _, name := range allowed {
			require.Containsf(t, offenders, name,
				"%s is allow-listed to touch motifs in %s but no longer does — "+
					"remove the entry rather than leaving a permission nothing uses", name, rel)
		}
		for _, fn := range offenders {
			require.Containsf(t, allowed, fn,
				"MN6: %s.%s is a mechanical decision path and must not read motifs. "+
					"If it WRITES facts rather than deciding something, allow-list it in "+
					"mechanicsPaths with a reason.", rel, fn)
		}
	}
}

// funcsMentioningMotifs returns the names of top-level functions in rel whose
// body mentions a motif identifier. Parsed, not grepped, so a comment
// explaining why a function has nothing to do with motifs does not trip it —
// and so the check can be scoped to a function rather than a whole file.
func funcsMentioningMotifs(t *testing.T, rel string) []string {
	t.Helper()
	var out []string
	for _, decl := range parseGo(t, rel).Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		mentions := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch id := n.(type) {
			case *ast.Ident:
				if strings.Contains(strings.ToLower(id.Name), "motif") {
					mentions = true
				}
			case *ast.SelectorExpr:
				if strings.Contains(strings.ToLower(id.Sel.Name), "motif") {
					mentions = true
				}
			case *ast.BasicLit:
				// String literals count (Phase 2). In internal/store every
				// mechanical decision this rule polices is expressed as SQL
				// TEXT, not as Go identifiers: a WHERE clause selecting on
				// motifs is a string, and an Ident-only walk cannot see it.
				// Without this case the check was fail-OPEN in exactly the
				// package where MN6 has the most to catch — a motif filter
				// added to newFactFilter's clause builder would have passed
				// silently while the allow-list said that file touched nothing.
				//
				// It costs some precision: a function whose SQL merely SELECTs
				// the column now needs an allow-list entry too. That is the
				// right trade — the entry states, in writing, that the function
				// carries motifs and does not decide with them.
				if id.Kind == token.STRING && strings.Contains(strings.ToLower(id.Value), "motif") {
					mentions = true
				}
			}
			return !mentions
		})
		if mentions {
			out = append(out, fn.Name.Name)
		}
	}
	return out
}

// TestMN2_NoLLMInMotifCode — vacuous this phase, and stated anyway so the
// phase-3 enumeration loop inherits a check that already exists rather than
// needing one written under deadline.
func TestMN2_NoLLMInMotifCode(t *testing.T) {
	sources := goSources(t)
	for _, rel := range []string{
		"internal/fact/motif.go", "internal/textnorm/textnorm.go",
		// Phase 3's enumeration and its gate. The detector stays mechanical:
		// the connected agent is the only reasoner in the read path, and it
		// already exists (cf455b8f).
		"internal/synthesize/bridge_motif.go", "internal/synthesize/motif_disjoint.go",
	} {
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
// path is a documented budget, never a corpus-property constant.
//
// Two things it deliberately does NOT do, both fixed after review:
//
//   - It does not assert the ORDER of comment prose. The first version required
//     "CONSTANT CLASSIFICATION" to appear before "const MaxMotifs" in the file
//     text, which is a rule about paragraph layout, not about the code — moving
//     a doc comment would have failed it while changing nothing that matters.
//     It now checks that the constant's own doc comment says what class it is.
//   - It does not regex the file for float literals. A float in a COMMENT
//     ("measured 0.75 on the research corpus") is calibration evidence being
//     cited, which MN13 explicitly permits; a float in the CODE is a threshold.
//     Only the parser can tell those apart.
func TestMN13_MotifConstantsAreClassified(t *testing.T) {
	const rel = "internal/fact/motif.go"
	file := parseGo(t, rel)

	// Each named constant must carry a doc comment declaring its class.
	wantClassified := map[string]string{
		"MaxMotifs":     "CONSTANT CLASSIFICATION",
		"minMotifWords": "contract, not",
		"maxMotifWords": "contract, not",
	}
	seen := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// A const block's doc comment covers every spec inside it.
		blockDoc := gen.Doc.Text()
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			doc := blockDoc + vs.Doc.Text()
			for _, name := range vs.Names {
				want, tracked := wantClassified[name.Name]
				if !tracked {
					continue
				}
				seen[name.Name] = true
				require.Containsf(t, doc, want,
					"MN13: const %s must state its class where it is defined "+
						"(expected its doc comment to contain %q)", name.Name, want)
			}
		}
	}
	for name := range wantClassified {
		require.Truef(t, seen[name], "const %s no longer exists in %s — "+
			"update this test rather than letting the classification rule lapse", name, rel)
	}

	// No float literal anywhere in the CODE. Comments are exempt by
	// construction: ast.Inspect walks expressions, not trivia.
	var floats []string
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.FLOAT {
			floats = append(floats, lit.Value)
		}
		return true
	})
	require.Emptyf(t, floats,
		"MN13: a float literal in the motif path is a corpus-property constant "+
			"in disguise: %v", floats)
}

// callsFunc reports whether the file at repo-relative rel contains an actual
// CALL to name (bare or selector-qualified), parsed rather than grepped, so
// comments and strings mentioning the name do not count.
func callsFunc(t *testing.T, rel, name string) bool {
	t.Helper()
	file := parseGo(t, rel)
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == name {
				found = true
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// parseGo parses one repo-relative Go file, failing (never skipping) if it
// cannot be read or parsed.
func parseGo(t *testing.T, rel string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), rel), nil, parser.ParseComments)
	require.NoErrorf(t, err, "parsing %s", rel)
	return file
}

// TestMN13_MotifBridgeConstantsAreClassified — the Phase-3 half of the same
// rule. Every numeric constant on the bridging path states its class where it
// is defined, and no float literal appears in either file's CODE: a float here
// would be the corpus-property constant MN13 forbids, wearing a threshold's
// clothes.
func TestMN13_MotifBridgeConstantsAreClassified(t *testing.T) {
	for rel, want := range map[string]map[string]string{
		"internal/synthesize/motif_disjoint.go": {
			"disjointnessPercentile": "SELECTION POINT",
			"minLabelsForPercentile": "STATISTICAL-VALIDITY FLOOR",
			"umbrellaPerCent":        "RATIO",
		},
		"internal/synthesize/bridge_motif.go": {
			"motifDFCeilingFloor":   "STATISTICAL-VALIDITY FLOOR",
			"motifDFCeilingPerCent": "RATIO",
			"motifActivationFloor":  "STATISTICAL-VALIDITY FLOOR",
			"token2SharedStems":     "STRUCTURAL K",
		},
	} {
		requireConstantsClassified(t, rel, want)
		requireEveryNumericConstantClassified(t, rel)
		requireNoUnnamedRatios(t, rel)
	}
}

// requireEveryNumericConstantClassified closes the opt-in hole (review finding
// M-6).
//
// requireConstantsClassified skips any constant absent from its map, so the
// phase added three numeric constants and the test that is named after the
// rule stayed green while none of them was checked — and the acceptance
// package claimed otherwise. Adding three map entries would satisfy the ruling
// once; making UNTRACKED constants fail stops the class recurring, which is
// what a conformance check is for.
//
// Scope is numeric LITERALS, which is MN13's own subject ("every numeric
// constant"). String enums and iota discriminants are excluded structurally
// rather than by an exemption list: they carry no tuned value, so there is no
// class to state, and a list would be one more thing to forget.
func requireEveryNumericConstantClassified(t *testing.T, rel string) {
	t.Helper()
	for _, decl := range parseGo(t, rel).Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		blockDoc := gen.Doc.Text()
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			numeric := false
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok &&
					(lit.Kind == token.INT || lit.Kind == token.FLOAT) {
					numeric = true
				}
			}
			if !numeric {
				continue
			}
			doc := blockDoc + vs.Doc.Text()
			for _, name := range vs.Names {
				require.Containsf(t, doc, "CONSTANT CLASSIFICATION",
					"MN13: numeric const %s in %s states no class. Every numeric constant is a "+
						"corpus-property constant (forbidden), a resource budget, or a "+
						"statistical-validity floor, and the class must be stated where it is "+
						"defined — a value whose paragraph is deleted is indistinguishable from "+
						"the forbidden kind", name.Name, rel)
			}
		}
	}
}

// requireConstantsClassified checks that each named constant in rel carries a
// doc comment declaring its class. Shared by both MN13 tests so the rule has
// one implementation and cannot drift between the phases that use it.
func requireConstantsClassified(t *testing.T, rel string, want map[string]string) {
	t.Helper()
	seen := map[string]bool{}
	for _, decl := range parseGo(t, rel).Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		blockDoc := gen.Doc.Text()
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			doc := blockDoc + vs.Doc.Text()
			for _, name := range vs.Names {
				phrase, tracked := want[name.Name]
				if !tracked {
					continue
				}
				seen[name.Name] = true
				require.Containsf(t, doc, phrase,
					"MN13: const %s in %s must state its class where it is defined "+
						"(expected its doc comment to contain %q)", name.Name, rel, phrase)
			}
		}
	}
	for name := range want {
		require.Truef(t, seen[name], "const %s no longer exists in %s — "+
			"update this test rather than letting the classification rule lapse", name, rel)
	}
}

// requireNoUnnamedRatios is MN13's literal check, in BOTH forms a ratio can
// take.
//
// A float literal was the only form the first version looked for, and the
// Phase-3 review (L2) found the gap by walking through it: the df band's
// ceiling was written `LiveFacts * 2 / 100` — a ratio of the corpus's own size,
// in bare integers, invisible to a float scan. That is lesson 3 turned on the
// check itself: ask in what form the violation would appear, and does the check
// read that form.
//
// So it bans float literals outright, and bans the integer-ratio SHAPE
// (`<expr> * N / M`) when the MULTIPLIER N is a bare number. N is the ratio
// someone chose and must therefore be named and classified; the divisor is the
// unit's own denominator — `x * umbrellaPerCent / 100` is what a correctly
// classified per-cent ratio looks like, and a `const hundred = 100` would be
// ceremony, not clarity.
func requireNoUnnamedRatios(t *testing.T, rel string) {
	t.Helper()
	file := parseGo(t, rel)

	var offenders []string
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.FLOAT {
			offenders = append(offenders, "float literal "+lit.Value)
			return true
		}
		// `x * N / M` parses as (x * N) / M — a division whose left operand is
		// a multiplication. Either bare number makes it an unnamed ratio.
		div, ok := n.(*ast.BinaryExpr)
		if !ok || div.Op != token.QUO {
			return true
		}
		mul, ok := div.X.(*ast.BinaryExpr)
		if !ok || mul.Op != token.MUL {
			return true
		}
		if lit, ok := mul.Y.(*ast.BasicLit); ok && lit.Kind == token.INT {
			offenders = append(offenders,
				"unnamed integer ratio "+lit.Value+"/"+exprText(div.Y))
		}
		return true
	})
	require.Emptyf(t, offenders,
		"MN13: %s must not carry an unclassified ratio — name it as a constant and "+
			"state its class where it is defined: %v", rel, offenders)
}

// exprText renders a small expression for an error message.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Value
	case *ast.Ident:
		return v.Name
	}
	return "?"
}
