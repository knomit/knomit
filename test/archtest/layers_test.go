package archtest

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
)

// enforceLayers is the single switch that turns TestLayers from report-only
// (violations are t.Log'd) into enforcing (violations fail the test). It is
// report-only while the layer directories do not exist yet: today's flat
// packages are mapped to their TARGET layer by legacyLayers below. The PR that
// finishes the moves flips this to true.
const enforceLayers = false

// Layer ranks. A package may import its own layer and any layer below it,
// never above; same-layer edges between distinct packages must be listed in
// sameLayerEdges (a new one is a reviewed decision).
const (
	layerPlatform   = iota // knows the OS and the binary, nothing about knomit
	layerDomain            // pure knomit concepts: no I/O, no SQLite, no git, no cgo
	layerFoundation        // knomit-wide configuration and identity, no corpus
	layerEngine            // one external resource each, wrapped behind a knomit API
	layerCorpus            // operations on mounted repositories
	layerEdge              // protocols facing the outside world
	layerApp               // composition root
)

var layerNames = []string{"platform", "domain", "foundation", "engine", "corpus", "edge", "app"}

const internalPrefix = "knomit/internal/"

// layerExempt packages are outside the layer table: test-only helpers that
// only _test.go files may import (enforced by test_signer_isolation_test.go).
var layerExempt = []string{"testsupport"}

// legacyLayers maps today's flat package paths (relative to knomit/internal/)
// to their TARGET layer. Longest prefix wins, so embeddings/params can sit in
// domain while embeddings sits in engine. Every entry is deleted when its
// package moves under its layer directory, because layerOf resolves
// internal/<layer>/... by directory first. When this map is empty the table has
// become a path-prefix rule and a package that is in neither place is reported
// as unmapped. Do not add entries for new packages: put them in a layer dir.
var legacyLayers = map[string]int{
	// domain
	"fact":              layerDomain, // includes fact/textnorm
	"okf":               layerDomain, // includes okf/source
	"embeddings/params": layerDomain, // becomes domain/modelparams
	// foundation
	"config":          layerFoundation,
	"auth":            layerFoundation,
	"pki":             layerFoundation, // includes pki/pkitest
	"serverkey":       layerFoundation,
	"serveraddr":      layerFoundation,
	"client/sessions": layerFoundation,
	// engine
	"store":      layerEngine, // includes store/git, store/migrate, store/nosignertest
	"embeddings": layerEngine,
	"llm":        layerEngine,
	// corpus
	"repos":       layerCorpus,
	"refs":        layerCorpus,
	"federate":    layerCorpus,
	"resolutions": layerCorpus,
	"synthesize":  layerCorpus,
	// edge
	"mcp":   layerEdge,
	"web":   layerEdge, // includes web/hal
	"oauth": layerEdge, // includes oauth/idp
}

// sameLayerEdges lists the allowed imports between two distinct packages of
// one layer, as "importer -> imported" with paths relative to knomit/internal/
// (the path as it is at the time of writing). An import between a package and
// its own subpackage (store -> store/git) is always allowed and is not listed.
var sameLayerEdges = map[string]bool{
	// platform
	"platform/diag -> platform/hostguard": true,
	"platform/diag -> platform/metrics":   true,
	// domain
	"okf -> fact":        true,
	"okf/source -> fact": true,
	// foundation
	"config -> auth":       true,
	"serveraddr -> auth":   true,
	"serveraddr -> config": true,
	// engine
	"store/git -> store/migrate": true,
	// The ctx-carried transaction (ctxKeyTx, Conn, BeginTxIfNeeded) is defined
	// by the git storer; the sqlite child re-exposes those helpers to the
	// store's sub-indices rather than redefining them.
	"store/internal/sqlite -> store/git": true,
	// The sessions child (ephemeral session DB, pipeline and tool indexes)
	// sits above sqlite in the store's child order (proposal 4.3: sessions ->
	// sqlite): the pipeline watermarks live in the MAIN db and are read
	// through the ctx-carried tx helper (sqlite.Conn).
	"store/internal/sessions -> store/internal/sqlite": true,
	// The vcs child (the git layer: branches, commits, locks, signing, commit
	// log, path history) sits on the git storer and directly above sqlite in
	// the store's child order (proposal 4.3); every higher cluster reaches up
	// to it, never the reverse. It reads and writes its tables through the
	// ctx-carried tx helpers (sqlite.Conn, sqlite.BeginTxIfNeeded).
	"store/internal/vcs -> store/git":             true,
	"store/internal/vcs -> store/internal/sqlite": true,
	// The fleetrec child (a fleet repository's member records, certificate
	// artifacts and main-history log, read from one commit) sits directly above
	// vcs in the store's child order (proposal 4.3: fleetrec -> vcs): it reads
	// the ontology at a commit and checks the tree shape through the vcs
	// primitives. verify and the facade reach up to it, never the reverse.
	"store/fleetrec -> store/internal/vcs": true,
	// The verify child (the judge: signatures, tree shape, the acceptance
	// range, M3/E4, validations, artifact rules, the incoming-push judge and
	// the audit) sits above vcs and fleetrec in the store's child order
	// (proposal 4.3: verify -> vcs, fleetrec). It reads a store only through
	// *vcs.Repo and the fleet's records through fleetrec; the sync, gitserve
	// and fsck clusters reach up to it, never the reverse.
	"store/verify -> store/fleetrec":     true,
	"store/verify -> store/internal/vcs": true,
	// The facts child (fact files read and written through the git layer, the
	// system-file / template / skill readers, the trigger dispatcher's store
	// half) sits above vcs and verify in the store's child order (proposal
	// 4.3: facts -> vcs, verify). It also reaches the sqlite child directly,
	// for the ctx-carried transaction its trigger tables use (sqlite is below
	// vcs in the order, so the direction is still downward); the merge, index,
	// sync and fsck clusters reach up to facts, never the reverse.
	"store/facts -> store/internal/sqlite": true,
	"store/facts -> store/internal/vcs":    true,
	"store/facts -> store/verify":          true,
	// The merge child (branch merges and the conflict strategies, the
	// pushed-branch and consensus merge cores, the experiment lifecycle) sits
	// above vcs and verify in the store's child order (proposal 4.3: merge ->
	// vcs, verify, facts). It reads a store only through *vcs.Repo, uses
	// verify for the merge-time validation, and reaches the sqlite child
	// directly for the ctx-carried transaction its experiments rows use (sqlite
	// is below vcs, so the direction is still downward). It does NOT import
	// facts: nothing in the moved files needs it. sync, gitserve and fsck reach
	// up to merge, never the reverse.
	"store/merge -> store/internal/sqlite": true,
	"store/merge -> store/internal/vcs":    true,
	"store/merge -> store/verify":          true,
	// The index child (the search index: the IndexManager lifecycle, the
	// write/rebuild/graph-write path and the vec tables, plus the read-side
	// query services FactQuery, GraphStore, HistoryQuery, MethodologyMatcher)
	// sits above sqlite, vcs and facts in the store's child order (proposal
	// 4.3: index -> sqlite, vcs, facts). Every one of its types reaches UP to
	// the one *vcs.Repo; it imports facts only for the FactRecord /
	// FactWithBody types and ParseFactRecord, and the git storer for the
	// CtxExecer type the graph SQL takes and WithoutTx (the same edge vcs
	// has). vocab and the facade reach up to index, never the reverse.
	"store/index -> store/facts":           true,
	"store/index -> store/git":             true,
	"store/index -> store/internal/sqlite": true,
	"store/index -> store/internal/vcs":    true,
	// The vocab child (motif alias resolution and definitions, the
	// title-embedding axis and its shortlist: review-time derived state) sits
	// above sqlite and vcs in the store's child order. The proposal's arrow
	// also lists index; nothing in the moved files needs it, so it is not
	// imported. It reads a store only through *vcs.Repo and the ctx-carried
	// tx helpers; the facade reaches up to it, never the reverse.
	"store/vocab -> store/internal/sqlite": true,
	"store/vocab -> store/internal/vcs":    true,
	// The remotesync child (the origin remote and its status row, the fetch, the
	// reconcile of the consensus branch and of the agent branch with the
	// rewind replay and its watermark, push and publish, the origin identity,
	// peer admission, the publish-time judge of this instance's own commits
	// and the unpublished-commit drop) sits above vcs, verify and merge in
	// the store's child order (proposal 4.3: remotesync -> vcs, verify, merge). It
	// reads a store through the merge handle (which wraps the one *vcs.Repo),
	// judges with verify, reads the fleet's records through fleetrec (the
	// own-range judge) and uses the git storer directly for the empty tree
	// the rewind replay builds on. gitserve and fsck reach up to remotesync, never
	// the reverse.
	"store/remotesync -> store/fleetrec":     true,
	"store/remotesync -> store/git":          true,
	"store/remotesync -> store/internal/vcs": true,
	"store/remotesync -> store/merge":        true,
	"store/remotesync -> store/verify":       true,
	// The gitserve child (the smart-HTTP handler: the advertisement, the pack
	// negotiation and upload-pack, and receive-pack with its quarantine, the
	// incoming-push judge, the all-or-nothing promote and the registration) sits
	// above vcs, verify and remotesync in the store's child order (proposal 4.3:
	// gitserve -> vcs, verify, remotesync). It reads a store through the one
	// *vcs.Repo, judges a push with verify, and moves a pushed branch through
	// remotesync's AdvanceBranchTo. Two edges go beyond the proposal's arrow and are
	// types only: fleetrec, for the Fleet a PushPolicy hands the judge (verify's
	// signature already names it), and merge, for the Mode constants a branch
	// move reports (ModeRewound, ModeNoop). The facade reaches up to gitserve,
	// never the reverse.
	"store/gitserve -> store/fleetrec":     true,
	"store/gitserve -> store/internal/vcs": true,
	"store/gitserve -> store/merge":        true,
	"store/gitserve -> store/remotesync":   true,
	"store/gitserve -> store/verify":       true,
	// corpus
	"federate -> repos":    true,
	"resolutions -> refs":  true,
	"resolutions -> repos": true,
	"synthesize -> refs":   true,
	"synthesize -> repos":  true,
	// edge
	"web -> mcp":   true,
	"web -> oauth": true,
}

// layerOf returns the layer rank of a knomit/internal/... package. exempt is
// true for the test-support tree; ok is false when the package maps to no
// layer, which is itself a finding.
func layerOf(pkg string) (rank int, exempt, ok bool) {
	rel, found := strings.CutPrefix(pkg, internalPrefix)
	if !found {
		return 0, false, false
	}
	for _, e := range layerExempt {
		if rel == e || strings.HasPrefix(rel, e+"/") {
			return 0, true, true
		}
	}
	// Layer directory rule: internal/<layer>/... (internal/app is the app layer).
	first, _, _ := strings.Cut(rel, "/")
	if i := slices.Index(layerNames, first); i >= 0 {
		return i, false, true
	}
	// Legacy table, longest prefix wins.
	best, bestLen := 0, -1
	for p, r := range legacyLayers {
		if (rel == p || strings.HasPrefix(rel, p+"/")) && len(p) > bestLen {
			best, bestLen = r, len(p)
		}
	}
	if bestLen < 0 {
		return 0, false, false
	}
	return best, false, true
}

// isFamily reports whether a and b are the same package or one is a
// subpackage of the other.
func isFamily(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// layerViolations checks a direct-import graph (importer -> imports) and
// returns one sorted message per problem: an import of a higher layer, an
// unlisted same-layer edge, or a package mapped to no layer. Only edges whose
// importer is a knomit/internal package are checked.
func layerViolations(graph map[string][]string, allowed map[string]bool) []string {
	var out []string
	unmapped := map[string]bool{}
	for importer, imports := range graph {
		if !strings.HasPrefix(importer, internalPrefix) {
			continue
		}
		ir, iex, iok := layerOf(importer)
		if !iok {
			unmapped[importer] = true
			continue
		}
		if iex {
			continue
		}
		for _, dep := range imports {
			if !strings.HasPrefix(dep, internalPrefix) {
				continue
			}
			dr, dex, dok := layerOf(dep)
			if !dok {
				unmapped[dep] = true
				continue
			}
			if dex {
				continue
			}
			switch {
			case dr > ir:
				out = append(out, fmt.Sprintf("%s (%s) imports %s (%s): upward import", importer, layerNames[ir], dep, layerNames[dr]))
			case dr == ir && !isFamily(importer, dep):
				edge := strings.TrimPrefix(importer, internalPrefix) + " -> " + strings.TrimPrefix(dep, internalPrefix)
				if !allowed[edge] {
					out = append(out, fmt.Sprintf("%s imports %s: unlisted same-layer edge in %s (%s)", importer, dep, layerNames[ir], edge))
				}
			}
		}
	}
	for p := range unmapped {
		out = append(out, p+": maps to no layer (put it inside a layer directory)")
	}
	sort.Strings(out)
	return out
}

// listImports returns the direct-import graph of every knomit package under
// internal/ for one build configuration.
func listImports(t *testing.T, env []string, tags string) map[string][]string {
	t.Helper()
	args := []string{"list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "knomit/internal/...")
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %v (env %v) failed: %v\n%s", args, env, err, out)
	}
	graph := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		graph[f[0]] = f[1:]
	}
	return graph
}

// TestLayersDetectsViolations is the falsifiability check for TestLayers: it
// feeds synthetic graphs and requires the exact offending pair to be reported.
// If layerViolations went blind, the real test would log nothing and look
// green.
func TestLayersDetectsViolations(t *testing.T) {
	const p = internalPrefix
	graph := map[string][]string{
		p + "fact":                   {p + "store", "fmt"},          // domain -> engine: upward
		p + "store":                  {p + "fact", p + "store/git"}, // fine: downward, and own subpackage
		p + "store/git":              nil,
		p + "refs":                   {p + "repos"}, // corpus -> corpus, unlisted
		p + "repos":                  {p + "store"}, // fine
		p + "mystery":                {p + "fact"},  // maps to no layer
		p + "testsupport/testsigner": {p + "store"}, // exempt importer
	}
	got := layerViolations(graph, nil)
	want := []string{
		p + "fact (domain) imports " + p + "store (engine): upward import",
		p + "mystery: maps to no layer (put it inside a layer directory)",
		p + "refs imports " + p + "repos: unlisted same-layer edge in corpus (refs -> repos)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("violations =\n%q\nwant\n%q", got, want)
	}
	// Listing the edge makes it pass; the rest is unchanged.
	got = layerViolations(graph, map[string]bool{"refs -> repos": true})
	if len(got) != 2 {
		t.Fatalf("with refs -> repos allowed want 2 violations, got %q", got)
	}
	// Layer-directory rule beats the legacy table.
	if r, _, ok := layerOf(p + "domain/anything"); !ok || r != layerDomain {
		t.Fatalf("internal/domain/anything = (%d, %v), want domain", r, ok)
	}
}

// TestLayers enforces the internal/ layer rule: layer(importer) >= layer(imported),
// same-layer edges only when listed in sameLayerEdges, and every package maps
// to a layer. See enforceLayers: while it is false violations are logged, not
// failed. Direct imports are checked (transitive follows by induction) for the
// default GOOS, -tags desktop, linux, windows and darwin, closing the
// host-GOOS-only gap.
func TestLayers(t *testing.T) {
	configs := []struct {
		name string
		env  []string
		tags string
	}{
		{"default", nil, ""},
		{"desktop", nil, "desktop"},
		{"linux", []string{"GOOS=linux", "CGO_ENABLED=1"}, ""},
		{"windows", []string{"GOOS=windows", "CGO_ENABLED=1"}, ""},
		{"darwin", []string{"GOOS=darwin", "CGO_ENABLED=1"}, ""},
	}
	// Sentinels: one real package per layer, so a wrong pattern cannot pass vacuously.
	sentinels := map[int]string{
		layerPlatform: "platform/logging", layerDomain: "fact", layerFoundation: "config",
		layerEngine: "store", layerCorpus: "repos", layerEdge: "web", layerApp: "app",
	}
	seen := map[string]bool{}
	for _, c := range configs {
		graph := listImports(t, c.env, c.tags)
		if len(graph) == 0 {
			t.Fatalf("[%s] go list matched nothing — the guard would pass vacuously", c.name)
		}
		for rank, rel := range sentinels {
			if _, ok := graph[internalPrefix+rel]; !ok {
				t.Fatalf("[%s] sentinel %s for layer %s is missing — the guard is not watching the tree", c.name, rel, layerNames[rank])
			}
			if r, _, ok := layerOf(internalPrefix + rel); !ok || r != rank {
				t.Fatalf("sentinel %s does not map to layer %s", rel, layerNames[rank])
			}
		}
		for _, v := range layerViolations(graph, sameLayerEdges) {
			if seen[v] {
				continue
			}
			seen[v] = true
			if enforceLayers {
				t.Errorf("[%s] %s", c.name, v)
			} else {
				t.Logf("[%s] LAYER VIOLATION (report-only): %s", c.name, v)
			}
		}
		t.Logf("[%s] checked %d packages", c.name, len(graph))
	}
	if !enforceLayers {
		t.Logf("report-only: %d distinct violation(s); flip enforceLayers to fail on them", len(seen))
	}
}
