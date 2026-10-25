package archtest

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

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

// sameLayerEdges lists the allowed imports between two distinct packages of
// one layer, as "importer -> imported" with paths relative to knomit/internal/
// (the path as it is at the time of writing). An import between a package and
// its own subpackage (engine/store -> engine/store/git) is always allowed and is not listed.
var sameLayerEdges = map[string]bool{
	// platform
	"platform/diag -> platform/hostguard": true,
	"platform/diag -> platform/metrics":   true,
	// domain
	"domain/okf -> domain/fact":        true,
	"domain/okf/source -> domain/fact": true,
	// foundation
	"foundation/config -> foundation/auth":       true,
	"foundation/serveraddr -> foundation/auth":   true,
	"foundation/serveraddr -> foundation/config": true,
	// engine
	"engine/store/git -> engine/store/migrate": true,
	// The ctx-carried transaction (ctxKeyTx, Conn, BeginTxIfNeeded) is defined
	// by the git storer; the sqlite child re-exposes those helpers to the
	// store's sub-indices rather than redefining them.
	"engine/store/internal/sqlite -> engine/store/git": true,
	// The sessions child (ephemeral session DB, pipeline and tool indexes)
	// sits above sqlite in the store's child order (proposal 4.3: sessions ->
	// sqlite): the pipeline watermarks live in the MAIN db and are read
	// through the ctx-carried tx helper (sqlite.Conn).
	"engine/store/sessions -> engine/store/internal/sqlite": true,
	// The vcs child (the git layer: branches, commits, locks, signing, commit
	// log, path history) sits on the git storer and directly above sqlite in
	// the store's child order (proposal 4.3); every higher cluster reaches up
	// to it, never the reverse. It reads and writes its tables through the
	// ctx-carried tx helpers (sqlite.Conn, sqlite.BeginTxIfNeeded).
	"engine/store/internal/vcs -> engine/store/git":             true,
	"engine/store/internal/vcs -> engine/store/internal/sqlite": true,
	// The fleetrec child (a fleet repository's member records, certificate
	// artifacts and main-history log, read from one commit) sits directly above
	// vcs in the store's child order (proposal 4.3: fleetrec -> vcs): it reads
	// the ontology at a commit and checks the tree shape through the vcs
	// primitives. verify and the facade reach up to it, never the reverse.
	"engine/store/fleetrec -> engine/store/internal/vcs": true,
	// The verify child (the judge: signatures, tree shape, the acceptance
	// range, M3/E4, validations, artifact rules, the incoming-push judge and
	// the audit) sits above vcs and fleetrec in the store's child order
	// (proposal 4.3: verify -> vcs, fleetrec). It reads a store only through
	// *vcs.Repo and the fleet's records through fleetrec; the sync, gitserve
	// and fsck clusters reach up to it, never the reverse.
	"engine/store/verify -> engine/store/fleetrec":     true,
	"engine/store/verify -> engine/store/internal/vcs": true,
	// The facts child (fact files read and written through the git layer, the
	// system-file / template / skill readers, the trigger dispatcher's store
	// half) sits above vcs and verify in the store's child order (proposal
	// 4.3: facts -> vcs, verify). It also reaches the sqlite child directly,
	// for the ctx-carried transaction its trigger tables use (sqlite is below
	// vcs in the order, so the direction is still downward); the merge, index,
	// sync and fsck clusters reach up to facts, never the reverse.
	"engine/store/facts -> engine/store/internal/sqlite": true,
	"engine/store/facts -> engine/store/internal/vcs":    true,
	"engine/store/facts -> engine/store/verify":          true,
	// The merge child (branch merges and the conflict strategies, the
	// pushed-branch and consensus merge cores, the experiment lifecycle) sits
	// above vcs and verify in the store's child order (proposal 4.3: merge ->
	// vcs, verify, facts). It reads a store only through *vcs.Repo, uses
	// verify for the merge-time validation, and reaches the sqlite child
	// directly for the ctx-carried transaction its experiments rows use (sqlite
	// is below vcs, so the direction is still downward). It does NOT import
	// facts: nothing in the moved files needs it. sync, gitserve and fsck reach
	// up to merge, never the reverse.
	"engine/store/merge -> engine/store/internal/sqlite": true,
	"engine/store/merge -> engine/store/internal/vcs":    true,
	"engine/store/merge -> engine/store/verify":          true,
	// The index child (the search index: the IndexManager lifecycle, the
	// write/rebuild/graph-write path and the vec tables, plus the read-side
	// query services FactQuery, GraphStore, HistoryQuery, MethodologyMatcher)
	// sits above sqlite, vcs and facts in the store's child order (proposal
	// 4.3: index -> sqlite, vcs, facts). Every one of its types reaches UP to
	// the one *vcs.Repo; it imports facts only for the FactRecord /
	// FactWithBody types and ParseFactRecord, and the git storer for the
	// CtxExecer type the graph SQL takes and WithoutTx (the same edge vcs
	// has). vocab and the facade reach up to index, never the reverse.
	"engine/store/index -> engine/store/facts":           true,
	"engine/store/index -> engine/store/git":             true,
	"engine/store/index -> engine/store/internal/sqlite": true,
	"engine/store/index -> engine/store/internal/vcs":    true,
	// The vocab child (motif alias resolution and definitions, the
	// title-embedding axis and its shortlist: review-time derived state) sits
	// above sqlite and vcs in the store's child order. The proposal's arrow
	// also lists index; nothing in the moved files needs it, so it is not
	// imported. It reads a store only through *vcs.Repo and the ctx-carried
	// tx helpers; the facade reaches up to it, never the reverse.
	"engine/store/vocab -> engine/store/internal/sqlite": true,
	"engine/store/vocab -> engine/store/internal/vcs":    true,
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
	"engine/store/remotesync -> engine/store/fleetrec":     true,
	"engine/store/remotesync -> engine/store/git":          true,
	"engine/store/remotesync -> engine/store/internal/vcs": true,
	"engine/store/remotesync -> engine/store/merge":        true,
	"engine/store/remotesync -> engine/store/verify":       true,
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
	"engine/store/gitserve -> engine/store/fleetrec":     true,
	"engine/store/gitserve -> engine/store/internal/vcs": true,
	"engine/store/gitserve -> engine/store/merge":        true,
	"engine/store/gitserve -> engine/store/remotesync":   true,
	"engine/store/gitserve -> engine/store/verify":       true,
	// The fsck child (the integrity checker behind Service.Integrity()
	// Verify and PruneGeneratedRefs) sits at the top of the store's child order (proposal
	// 4.3: fsck -> everything above): it reads the one *vcs.Repo and the two
	// sub-indices its checks need, the fact index for the fact-format check and
	// the search index for the parity and embedding checks. Nothing in the
	// store imports it but the facade.
	"engine/store/fsck -> engine/store/facts":        true,
	"engine/store/fsck -> engine/store/index":        true,
	"engine/store/fsck -> engine/store/internal/vcs": true,
	// The storetest package (test doors for tests outside the store) reaches
	// the vcs repo through vcs's registration hook and the sqlite durability
	// switch; it reaches the facts index and the merge repo through
	// Service.Facts and Service.Merge, so it imports no facts or merge package. It is a
	// test package; TestHarnessNotLinkedIntoShippedBinaries keeps it out of
	// every binary and TestTestSeamsNotReachedFromProduction out of production
	// imports.
	"engine/store/storetest -> engine/store/internal/sqlite": true,
	"engine/store/storetest -> engine/store/internal/vcs":    true,
	// store/model is the leaf every store child may import: plain data types,
	// sentinels and pure helpers. It imports nothing from the store
	// (TestStoreModel_IsALeaf), so these edges can never close a cycle.
	"engine/store/facts -> engine/store/model":           true,
	"engine/store/fsck -> engine/store/model":            true,
	"engine/store/index -> engine/store/model":           true,
	"engine/store/sessions -> engine/store/model":        true,
	"engine/store/internal/sqlite -> engine/store/model": true,
	"engine/store/internal/vcs -> engine/store/model":    true,
	"engine/store/merge -> engine/store/model":           true,
	"engine/store/remotesync -> engine/store/model":      true,
	// corpus
	"corpus/federate -> corpus/repos":    true,
	"corpus/resolutions -> corpus/refs":  true,
	"corpus/resolutions -> corpus/repos": true,
	"corpus/synthesize -> corpus/repos":  true,
	// The trigger child (the F07 dispatcher, 14c) is a sibling of events
	// under corpus/repos: its emit publishes the `trigger` event
	// (events.TriggerEvent) through the *events.TaskPublisher the parent hands
	// it. (Not a parent edge: the parent imports trigger, never the reverse.)
	"corpus/repos/trigger -> corpus/repos/events": true,
	// The step child holds the vocabulary the synthesize engine and its
	// strategies share: Deps carries the repo instance, and the write gate
	// builds a ref gate.
	"corpus/synthesize/step -> corpus/refs":  true,
	"corpus/synthesize/step -> corpus/repos": true,
	// The restatement child plans and judges the corpus-wide shortlist; its
	// work items and Deps are step vocabulary. (Not a parent edge: the parent
	// imports restatement, never the reverse.)
	"corpus/synthesize/restatement -> corpus/synthesize/step": true,
	// The graph child clusters step.FactForLLM values (step vocabulary).
	"corpus/synthesize/graph -> corpus/synthesize/step": true,
	// The motif child plans and decodes the vocabulary steps over step.Deps and
	// work items (step vocabulary).
	"corpus/synthesize/motif -> corpus/synthesize/step": true,
	// The bridge child enumerates and scores bridges over step.FactForLLM and
	// graph clusters; QualityConfigFromRepo reads the repo instance's settings.
	"corpus/synthesize/bridge -> corpus/repos":            true,
	"corpus/synthesize/bridge -> corpus/synthesize/graph": true,
	"corpus/synthesize/bridge -> corpus/synthesize/step":  true,
	// The discovery child ingests discover work items: it reads the bridge seed
	// sets the bridge child builds, rides step vocabulary, and resolves the repo
	// instance for gates and ref canonicalisation.
	"corpus/synthesize/discovery -> corpus/repos":             true,
	"corpus/synthesize/discovery -> corpus/synthesize/bridge": true,
	"corpus/synthesize/discovery -> corpus/synthesize/step":   true,
	// synthtest is the test-support package of the synthesize tree; its
	// environment builders stand up a repo instance.
	"corpus/synthesize/internal/synthtest -> corpus/repos":           true,
	"corpus/synthesize/internal/synthtest -> corpus/synthesize/step": true,
	// edge
	"edge/web -> edge/mcp":   true,
	"edge/web -> edge/oauth": true,
	// httpx is a sibling of hal under edge/web, not a subpackage of it: every
	// problem document it writes goes through hal.WriteProblem.
	"edge/web/httpx -> edge/web/hal": true,
	// api/origins (12d) is a sibling of httpx and hal under edge/web: it
	// writes problems and HAL through hal and reaches the shared helpers
	// (APIBase, DecodeJSON, StartSSE, ...) through httpx.
	"edge/web/api/origins -> edge/web/hal":   true,
	"edge/web/api/origins -> edge/web/httpx": true,
	// api/jobs (12c) is the same: problems and HAL through hal, the shared
	// helpers (APIBase, StartSSE, RefuseUnwritableBranch, ...) through httpx.
	"edge/web/api/jobs -> edge/web/hal":   true,
	"edge/web/api/jobs -> edge/web/httpx": true,
	// api/manage (12e) is the same: problems and HAL through hal, the shared
	// helpers (APIBase, DecodeJSON, WriteLifecycleProblem, ...) through httpx.
	"edge/web/api/manage -> edge/web/hal":   true,
	"edge/web/api/manage -> edge/web/httpx": true,
	// api/kb (12f) is the same: problems and HAL through hal, the shared
	// helpers (APIBase, BranchFromContext, WriteStoreError, ...) through httpx.
	"edge/web/api/kb -> edge/web/hal":   true,
	"edge/web/api/kb -> edge/web/httpx": true,
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
	// A package in no layer directory maps to no layer, which is a finding.
	first, _, _ := strings.Cut(rel, "/")
	if i := slices.Index(layerNames, first); i >= 0 {
		return i, false, true
	}
	return 0, false, false
}

// isKnomit reports whether an import path is a package of this module.
func isKnomit(path string) bool {
	return path == "knomit" || strings.HasPrefix(path, "knomit/")
}

// isFamily reports whether a and b are the same package or one is a
// subpackage of the other.
func isFamily(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// layerViolations checks a direct-import graph (importer -> imports) and
// returns one sorted message per problem: an import of a higher layer, an
// unlisted same-layer edge, a package mapped to no layer, or an import of a
// knomit package outside internal/ that itself imports knomit (it could carry
// the importer to a higher layer, so it is not a leaf). Only edges whose
// importer is a knomit/internal package are checked.
func layerViolations(graph map[string][]string, allowed map[string]bool) []string {
	out, _ := checkLayers(graph, allowed)
	return out
}

// checkLayers is layerViolations that also returns every same-layer edge
// ("importer -> imported", relative to knomit/internal/) it observed, allowed
// or not, so TestLayers can require each sameLayerEdges entry to be live.
func checkLayers(graph map[string][]string, allowed map[string]bool) ([]string, map[string]bool) {
	var out []string
	edges := map[string]bool{}
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
				if isKnomit(dep) {
					if sub, listed := graph[dep]; !listed {
						out = append(out, fmt.Sprintf("%s imports %s: outside package is not in the listing", importer, dep))
					} else if i := slices.IndexFunc(sub, isKnomit); i >= 0 {
						out = append(out, fmt.Sprintf("%s imports %s, which imports %s: a non-internal package may be imported from internal/ only if it imports nothing from knomit", importer, dep, sub[i]))
					}
				}
				continue
			}
			dr, dex, _ := layerOf(dep)
			if dex {
				continue
			}
			switch {
			case dr > ir:
				out = append(out, fmt.Sprintf("%s (%s) imports %s (%s): upward import", importer, layerNames[ir], dep, layerNames[dr]))
			case dr == ir && !isFamily(importer, dep):
				edge := strings.TrimPrefix(importer, internalPrefix) + " -> " + strings.TrimPrefix(dep, internalPrefix)
				edges[edge] = true
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
	return out, edges
}

// Deliberate: go list .Imports covers non-test files only, so _test.go imports
// are never checked. Tests are out of scope by decision; only shipped code is
// held to the layers.
// listImports returns the direct-import graph of every package of the knomit
// module for one build configuration (internal and not, so an internal package
// cannot reach a higher layer through a package outside internal/).
func listImports(t *testing.T, env []string, tags string) map[string][]string {
	t.Helper()
	args := []string{"list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "knomit/...")
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
		p + "domain/fact":            {p + "engine/store", "fmt"},                 // domain -> engine: upward
		p + "engine/store":           {p + "domain/fact", p + "engine/store/git"}, // fine: downward, and own subpackage
		p + "engine/store/git":       nil,
		p + "corpus/refs":            {p + "corpus/repos"},           // corpus -> corpus, unlisted
		p + "corpus/repos":           {p + "engine/store"},           // fine
		p + "mystery":                {p + "domain/fact"},            // maps to no layer
		p + "testsupport/testsigner": {p + "engine/store"},           // exempt importer
		p + "edge/mcp":               {p + "testsupport/testsigner"}, // exempt dependency
		p + "platform/hostguard":     {"knomit/tools/bridge"},        // launders: the outside package imports knomit
		p + "edge/web":               {"knomit/web"},                 // fine: outside leaf (imports nothing from knomit)
		p + "app/wire":               {"knomit/tools/ghost"},         // outside package missing from the listing
		"knomit/tools/bridge":        {p + "foundation/config"},
		"knomit/web":                 {"embed"},
	}
	got := layerViolations(graph, nil)
	want := []string{
		p + "app/wire imports knomit/tools/ghost: outside package is not in the listing",
		p + "corpus/refs imports " + p + "corpus/repos: unlisted same-layer edge in corpus (corpus/refs -> corpus/repos)",
		p + "domain/fact (domain) imports " + p + "engine/store (engine): upward import",
		p + "mystery: maps to no layer (put it inside a layer directory)",
		p + "platform/hostguard imports knomit/tools/bridge, which imports " + p + "foundation/config: a non-internal package may be imported from internal/ only if it imports nothing from knomit",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("violations =\n%q\nwant\n%q", got, want)
	}
	// Listing the edge makes it pass; the rest is unchanged.
	got = layerViolations(graph, map[string]bool{"corpus/refs -> corpus/repos": true})
	if len(got) != 4 {
		t.Fatalf("with corpus/refs -> corpus/repos allowed want 4 violations, got %q", got)
	}
	// Layer-directory rule.
	if r, _, ok := layerOf(p + "domain/anything"); !ok || r != layerDomain {
		t.Fatalf("internal/domain/anything = (%d, %v), want domain", r, ok)
	}
}

// TestLayers enforces the internal/ layer rule: layer(importer) >= layer(imported),
// same-layer edges only when listed in sameLayerEdges, every package maps to a
// layer directory, and every sameLayerEdges entry is still an edge of the real
// import graph in at least one configuration (a stale entry fails, so the
// allowlist cannot silently widen). Direct imports are checked (transitive
// follows by induction) for the default GOOS, -tags desktop, linux, windows and
// darwin, closing the host-GOOS-only gap (the desktop tag is listed for
// completeness; no internal file carries that constraint today). The union of
// the configurations must cover every directory under internal/ that holds a
// .go file, so a package hidden from every configuration fails by name.
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
		layerPlatform: "platform/logging", layerDomain: "domain/fact", layerFoundation: "foundation/config",
		layerEngine: "engine/store", layerCorpus: "corpus/repos", layerEdge: "edge/web", layerApp: "app",
	}
	seen := map[string]bool{}
	observed := map[string]bool{}
	listed := map[string]bool{}
	for _, c := range configs {
		graph := listImports(t, c.env, c.tags)
		for rank, rel := range sentinels {
			if _, ok := graph[internalPrefix+rel]; !ok {
				t.Fatalf("[%s] sentinel %s for layer %s is missing — the guard is not watching the tree", c.name, rel, layerNames[rank])
			}
			if r, _, ok := layerOf(internalPrefix + rel); !ok || r != rank {
				t.Fatalf("sentinel %s does not map to layer %s", rel, layerNames[rank])
			}
		}
		for pkg := range graph {
			if strings.HasPrefix(pkg, internalPrefix) {
				listed[pkg] = true
			}
		}
		violations, edges := checkLayers(graph, sameLayerEdges)
		for e := range edges {
			observed[e] = true
		}
		for _, v := range violations {
			if seen[v] {
				continue
			}
			seen[v] = true
			t.Errorf("[%s] %s", c.name, v)
		}
		t.Logf("[%s] checked %d packages", c.name, len(graph))
	}
	// Structural floor: every internal/ directory with a .go file is a package
	// some configuration must have listed. A directory whose .go files are all
	// excluded from every configuration (for instance every file is
	// `//go:build ignore`) fails here too, and no configuration can fix that:
	// the message says so.
	for _, dir := range internalGoDirs(t, "../../internal") {
		if !listed[internalPrefix+dir] {
			t.Errorf("internal/%s holds .go files but no configuration lists it as a package — either a configuration that builds it is missing from this test, or every .go file there is excluded from every build (for instance `//go:build ignore`), in which case it is not a package: move the files out of internal/ or give the directory a buildable file", dir)
		}
	}
	for pkg := range listed {
		if _, err := os.Stat(filepath.Join("../../internal", strings.TrimPrefix(pkg, internalPrefix))); err != nil {
			t.Errorf("%s was listed but has no directory under internal/: %v", pkg, err)
		}
	}
	for e := range sameLayerEdges {
		if !observed[e] {
			t.Errorf("sameLayerEdges entry %q is stale: no configuration's import graph has that edge — delete it", e)
		}
	}
}

// internalGoDirs returns, relative to root, every directory that contains a
// .go file, skipping testdata and directories whose name starts with "_" or ".".
// It looks at file names only, not build constraints, so a directory whose files
// are all `//go:build ignore` is returned although no configuration lists it as a
// package; TestLayers' structural floor reports that case.
func internalGoDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") {
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			if !slices.Contains(dirs, filepath.ToSlash(rel)) {
				dirs = append(dirs, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(dirs) == 0 {
		t.Fatalf("no Go directories under %s — the structural floor would pass vacuously", root)
	}
	return dirs
}
