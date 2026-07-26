// internal/okf/bundle_test.go
package okf

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func factInput(t *testing.T, path, content string, ts time.Time) FactInput {
	return FactInput{Fact: mkFact(t, path, content), Timestamp: ts}
}

func bundleMap(b Bundle) map[string]string {
	m := map[string]string{}
	for _, f := range b.Files {
		m[f.Path] = string(f.Content)
	}
	return m
}

func TestBuild_StructureAndIndexes(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	f1 := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf]
---
# Export scope is repo only

Body.`, ts)
	f2 := factInput(t, "kb/invariants/okf/refs-never-pushed/3209d651.md", `---
kind: pragmatic
type: policy
domain: [okf]
---
# Refs never pushed

Body.`, ts)

	b, skips := Build(RepoIdentity{ID: "0123456789ab"},
		[]FactInput{f1, f2},
		[]LogEntry{{Date: ts, Kind: "Creation", Title: "Export scope is repo only", Path: "kb/decisions/okf/scope/d9d6557d.md"}},
		RenderOpts{})
	if skips.Skipped != 0 {
		t.Fatalf("unexpected skips: %+v", skips)
	}
	m := bundleMap(b)

	// Concept doc paths: kb/ dropped, directories preserved, slug+uuid filename.
	if _, ok := m["kb/decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md"]; !ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("concept path missing; have:\n%s", strings.Join(keys, "\n"))
	}
	if _, ok := m["kb/invariants/okf/refs-never-pushed/refs-never-pushed-3209d651.md"]; !ok {
		t.Fatal("second concept path missing")
	}

	// Reserved root files.
	root, ok := m["index.md"]
	if !ok || !strings.Contains(root, `okf_version: "0.2"`) {
		t.Fatalf("root index.md missing okf_version:\n%s", root)
	}
	if _, ok := m["log.md"]; !ok {
		t.Fatal("log.md missing")
	}

	// Per-directory index.md exists at each level and lists children.
	for _, p := range []string{
		"kb/decisions/index.md",
		"kb/decisions/okf/index.md",
		"kb/decisions/okf/scope/index.md",
		"kb/invariants/index.md",
	} {
		if _, ok := m[p]; !ok {
			t.Errorf("missing index: %s", p)
		}
	}
	// A leaf directory's index must LINK the concept, not just name it —
	// index.md is OKF's progressive-disclosure surface, so a consumer (or a
	// human browsing on GitHub) has to be able to reach the document from it.
	leaf := m["kb/decisions/okf/scope/index.md"]
	// Links are RELATIVE (a sibling document is just its filename), because
	// GitHub resolves a leading "/" against the repo root and publishing there
	// is the intended distribution path.
	wantEntry := "- [Export scope is repo only](export-scope-is-repo-only-d9d6557d.md) — decision\n"
	if !strings.Contains(leaf, wantEntry) {
		t.Errorf("leaf index missing linked concept entry %q:\n%s", wantEntry, leaf)
	}
	// A non-root index.md must NOT carry frontmatter (okf_version only at root).
	if strings.Contains(leaf, "okf_version") {
		t.Errorf("non-root index must not carry okf_version:\n%s", leaf)
	}
}

// TestBuild_IndexLinkEscapesBracketsInTitle pins the escaping of markdown
// link-label delimiters. Real knomit titles contain brackets (e.g. "no
// skipped[] block", "refs → [:DERIVED_FROM] edges"), which would terminate the
// label early and produce a broken link in index.md.
func TestBuild_IndexLinkEscapesBracketsInTitle(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	f := factInput(t, "kb/invariants/store/edges/eb438c74.md", `---
kind: pragmatic
type: policy
domain: [store]
---
# refs → [:DERIVED_FROM] edges are driven from rec.Refs

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{f}, nil, RenderOpts{})
	idx := bundleMap(b)["kb/invariants/store/edges/index.md"]
	if !strings.Contains(idx, `\[:DERIVED_FROM\]`) {
		t.Errorf("brackets in title must be escaped in the link label:\n%s", idx)
	}
	// The link target must still be intact and reachable (relative sibling).
	if !strings.Contains(idx, "](refs-derived-from-edges-are-driven-from-rec-refs-eb438c74.md)") {
		t.Errorf("link target malformed:\n%s", idx)
	}
}

// TestBuild_CitationsLinkInternalFactsAndURLs is the core navigability test:
// a fact's refs are knomit's derivation graph, and unless they resolve to
// links the graph is invisible in the export.
func TestBuild_CitationsLinkInternalFactsAndURLs(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	target := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf]
---
# Export scope is repo only

Body.`, ts)
	citing := factInput(t, "kb/invariants/okf/refs-never-pushed/3209d651.md", `---
kind: pragmatic
type: policy
domain: [okf]
refs: ["kb/decisions/okf/scope/d9d6557d.md", "https://github.com/knomit/knomit/pull/20", "src://knomit/internal/store/remote_sync.go@abc1234"]
---
# Refs never pushed

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{target, citing}, nil, RenderOpts{})
	doc := bundleMap(b)["kb/invariants/okf/refs-never-pushed/refs-never-pushed-3209d651.md"]

	// An internal fact edge resolves to the TARGET's bundle document, relative,
	// and is LABELLED with the target's title — a raw fact path tells the
	// reader nothing about what they would be opening.
	wantInternal := "- [Export scope is repo only](../../../decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md)"
	if !strings.Contains(doc, wantInternal) {
		t.Errorf("internal fact edge not linked by title (want %q):\n%s", wantInternal, doc)
	}
	// The same title reaches the machine-readable v0.2 sources entry.
	if !strings.Contains(doc, "title: Export scope is repo only") {
		t.Errorf("sources entry missing the cited fact's title:\n%s", doc)
	}
	// An external URL becomes a real link.
	if !strings.Contains(doc, "](https://github.com/knomit/knomit/pull/20)") {
		t.Errorf("external URL not linked:\n%s", doc)
	}
	// src:// stays inert with no resolver — never a guessed link.
	if !strings.Contains(doc, "`src://knomit/internal/store/remote_sync.go@abc1234`") {
		t.Errorf("unresolvable src:// must stay inert:\n%s", doc)
	}
}

// TestBuild_DomainHubGroupsFacts covers the cross-cutting view OKF has no
// native structure for: "show me every fact in domain X". Hubs are ordinary
// conformant concept documents, so they cost nothing at consumption time.
func TestBuild_DomainHubGroupsFacts(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	a := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf, store]
---
# Export scope is repo only

Body.`, ts)
	b2 := factInput(t, "kb/invariants/okf/refs-never-pushed/3209d651.md", `---
kind: pragmatic
type: policy
domain: [okf]
---
# Refs never pushed

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{a, b2}, nil, RenderOpts{})
	m := bundleMap(b)

	hub, ok := m["views/domains/okf.md"]
	if !ok {
		t.Fatalf("domain hub missing; have: %v", sortedBundleKeys(m))
	}
	// Conformant concept: non-empty type.
	if !strings.Contains(hub, "type: Domain Overview") {
		t.Errorf("hub is not a conformant concept:\n%s", hub)
	}
	// Both facts are linked, relative to the hub's own directory.
	for _, want := range []string{
		"](../../kb/decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md)",
		"](../../kb/invariants/okf/refs-never-pushed/refs-never-pushed-3209d651.md)",
	} {
		if !strings.Contains(hub, want) {
			t.Errorf("hub missing member link %q:\n%s", want, hub)
		}
	}
	// A domain carrying a single fact gets NO page — a hub whose entire body is
	// one link is a wasted click. The index links that fact directly instead,
	// so every domain stays answerable.
	if _, ok := m["views/domains/store.md"]; ok {
		t.Error("single-fact domain should not get its own hub page")
	}
	if !strings.Contains(m["views/domains/index.md"], "[store](../../kb/decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md)") {
		t.Errorf("single-fact domain must link straight to its fact:\n%s", m["views/domains/index.md"])
	}
	// The hub directory is indexed and reachable from the root.
	if _, ok := m["views/domains/index.md"]; !ok {
		t.Error("domains/index.md missing")
	}
	if !strings.Contains(m["index.md"], "[views](views/index.md)") {
		t.Errorf("root index does not link the domains hub:\n%s", m["index.md"])
	}
}

// TestConcept_V02TrustFields pins the OKF v0.2 provenance mapping, including
// the deliberate refusal to claim human verification.
func TestConcept_V02TrustFields(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	// origin=distilled is only valid on a synthesis fact — knomit normalizes it
	// to "authored" otherwise, which would silently weaken this test.
	f := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: synthesis
domain: [okf]
origin: distilled
confidence: 0.9
refs: ["https://example.com/x"]
---
# Distilled fact

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{f}, nil, RenderOpts{})
	doc := bundleMap(b)["kb/decisions/okf/scope/distilled-fact-d9d6557d.md"]

	if !strings.Contains(doc, "generated:") || !strings.Contains(doc, "by: process:knomit-distill") {
		t.Errorf("generated.by not mapped from origin:\n%s", doc)
	}
	// A followable ref becomes a v0.2 sources entry.
	if !strings.Contains(doc, "sources:") || !strings.Contains(doc, "resource: https://example.com/x") {
		t.Errorf("sources entry missing:\n%s", doc)
	}
	// knomit has no verification events; claiming them would inflate the
	// consumer-derived trust tier on evidence we do not have.
	if strings.Contains(doc, "verified:") {
		t.Errorf("must not emit verified — knomit records no verification events:\n%s", doc)
	}
	// Confident, non-hypothesis fact ⇒ status absent ⇒ stable per spec.
	if strings.Contains(doc, "status:") {
		t.Errorf("status should be absent (⇒ stable) for a confident fact:\n%s", doc)
	}
}

// TestBuild_CitedBySection makes the derivation graph traversable in BOTH
// directions. A fact currently advertises what it cites but not what cites it,
// so the graph can only be walked one way.
func TestBuild_CitedBySection(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	target := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf]
---
# Export scope is repo only

Body.`, ts)
	citer := factInput(t, "kb/invariants/okf/refs-never-pushed/3209d651.md", `---
kind: pragmatic
type: policy
domain: [okf]
refs: ["kb/decisions/okf/scope/d9d6557d.md"]
---
# Refs never pushed

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{target, citer}, nil, RenderOpts{})
	m := bundleMap(b)

	// The CITED fact names its citer, by title, linked relatively.
	cited := m["kb/decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md"]
	want := "- [Refs never pushed](../../../invariants/okf/refs-never-pushed/refs-never-pushed-3209d651.md)"
	if !strings.Contains(cited, "# Cited by") || !strings.Contains(cited, want) {
		t.Errorf("cited fact missing incoming edge %q:\n%s", want, cited)
	}
	// The citing fact has no incoming edges, so no section.
	citing := m["kb/invariants/okf/refs-never-pushed/refs-never-pushed-3209d651.md"]
	if strings.Contains(citing, "# Cited by") {
		t.Errorf("fact with no incoming edges must not emit the section:\n%s", citing)
	}
}

// TestBuild_MethodologyDigest completes the fabricated-type digests. knomit's
// reflect pipeline produces methodology facts — knowledge about how to reason —
// which is a category no other OKF producer emits.
func TestBuild_MethodologyDigest(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	f := factInput(t, "kb/meta/reasoning/aa11bb22.md", `---
kind: epistemic
type: methodology
domain: [meta]
---
# Check the corpus before assuming

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{f}, nil, RenderOpts{})
	m := bundleMap(b)
	dig, ok := m["views/methodology.md"]
	if !ok {
		t.Fatalf("methodology digest missing; have %v", sortedBundleKeys(m))
	}
	if !strings.Contains(dig, "type: Methodology Digest") {
		t.Errorf("digest is not a conformant concept:\n%s", dig)
	}
	if !strings.Contains(m["views/index.md"], "[methodology](methodology.md)") {
		t.Errorf("views index does not list the methodology digest:\n%s", m["views/index.md"])
	}
}

func sortedBundleKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// TestBuild_OntologyDescriptionsEnrichIndexes covers the authored
// documentation the ontology already carries at every level. It is the honest
// source for the `description` OKF recommends — written by a human, not
// synthesized from a fact body.
func TestBuild_OntologyDescriptionsEnrichIndexes(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	f := factInput(t, "kb/principles/mission/aa11bb22.md", `---
kind: epistemic
type: principle
domain: [knomit]
---
# Why knomit exists

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{f}, nil, RenderOpts{
		Ontology: OntologyDoc{
			Name:        "Source Code Knowledge",
			Description: "Knowledge categories for AI agents working in a codebase.",
			Nodes: map[string]string{
				"principles":         "Designer-authored intent — mission, philosophy, anti-patterns, UX taste",
				"principles/mission": "What knomit exists to solve, who it's for",
			},
		},
	})
	m := bundleMap(b)

	// The knowledge root is titled and described by the ontology itself.
	root := m["kb/index.md"]
	if !strings.Contains(root, "# Source Code Knowledge") {
		t.Errorf("kb index not titled from the ontology:\n%s", root)
	}
	if !strings.Contains(root, "Knowledge categories for AI agents working in a codebase.") {
		t.Errorf("kb index missing the scheme description:\n%s", root)
	}
	// Entries carry the child's description, per the spec's index format.
	if !strings.Contains(root, "- [principles](principles/index.md) — Designer-authored intent") {
		t.Errorf("topic entry missing its description:\n%s", root)
	}
	// A topic index carries its own description as prose, and its categories'.
	topic := m["kb/principles/index.md"]
	if !strings.Contains(topic, "Designer-authored intent — mission, philosophy, anti-patterns, UX taste") {
		t.Errorf("topic index missing prose description:\n%s", topic)
	}
	if !strings.Contains(topic, "- [mission](mission/index.md) — What knomit exists to solve") {
		t.Errorf("category entry missing its description:\n%s", topic)
	}
	// Generated views are outside the authored ontology and get no prose.
	if strings.Contains(m["views/index.md"], "Knowledge categories") {
		t.Errorf("views must not inherit the ontology description:\n%s", m["views/index.md"])
	}
}

// TestBuild_AlphaIndexAndDigests covers the two navigation aids for large
// bundles: letter sections on a long hub index, and the single-file
// chronological digests over the higher-order fact types.
func TestBuild_AlphaIndexAndDigests(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	var facts []FactInput

	// Enough distinct entities (each on >= entityHubMinFacts facts) to push the
	// entity index past the flat-list threshold.
	letters := "abcdefghijklmnopqrstuvwxyz"
	for i, c := range letters {
		ent := string(c) + "sym"
		for n := 0; n < entityHubMinFacts; n++ {
			facts = append(facts, factInput(t,
				"kb/architecture/mod/"+string(c)+strconv.Itoa(n)+"aabbcc.md", `---
kind: epistemic
type: observation
domain: [store]
entities: [`+ent+`]
---
# Fact `+string(c)+strconv.Itoa(n)+`

Body.`, ts.AddDate(0, 0, i)))
		}
	}
	// One synthesis fact, on a distinct day.
	facts = append(facts, factInput(t, "kb/architecture/store/ff00aa11.md", `---
kind: epistemic
type: synthesis
domain: [store]
origin: distilled
---
# A distilled conclusion

Body.`, ts.AddDate(0, 0, 40)))

	b, _ := Build(RepoIdentity{ID: "x"}, facts, nil, RenderOpts{})
	m := bundleMap(b)

	// Long index gets a jump bar and letter sections.
	ents := m["views/entities/index.md"]
	if !strings.Contains(ents, "**Jump to:** [A](#a)") {
		t.Errorf("entity index missing alphabetical jump bar:\n%s", ents)
	}
	if !strings.Contains(ents, "\n## A\n") || !strings.Contains(ents, "\n## Z\n") {
		t.Errorf("entity index missing letter sections:\n%s", ents)
	}

	// The synthesis digest is a single conformant page, grouped by day.
	dig, ok := m["views/synthesis.md"]
	if !ok {
		t.Fatalf("synthesis digest missing; have %v", sortedBundleKeys(m))
	}
	if !strings.Contains(dig, "type: Synthesis Digest") {
		t.Errorf("digest is not a conformant concept:\n%s", dig)
	}
	// Two-level chronology: months in the jump bar (bounded as the KB grows),
	// days as subsections within.
	day := ts.AddDate(0, 0, 40).UTC().Format("2006-01-02")
	month := day[:7]
	if !strings.Contains(dig, "**Months:** ["+month+"](#"+month+") (1)") {
		t.Errorf("digest missing month jump bar with count:\n%s", dig)
	}
	if !strings.Contains(dig, "\n## "+month+"\n") {
		t.Errorf("digest missing month section:\n%s", dig)
	}
	if !strings.Contains(dig, "\n### "+day+"\n") {
		t.Errorf("digest missing day subsection:\n%s", dig)
	}
	if !strings.Contains(dig, "](../kb/architecture/store/a-distilled-conclusion-ff00aa11.md)") {
		t.Errorf("digest entry does not link its fact:\n%s", dig)
	}
	if !strings.Contains(dig, "1 fact.") {
		t.Errorf("digest count should agree in number:\n%s", dig)
	}

	// No hypothesis facts ⇒ no empty page, and views/index.md lists what exists.
	if _, ok := m["views/hypotheses.md"]; ok {
		t.Error("a digest with no facts must not be generated")
	}
	// Digest labels are lowercase, matching the directory entries beside them.
	vi := m["views/index.md"]
	if !strings.Contains(vi, "- [synthesis](synthesis.md) — 1 fact") {
		t.Errorf("views index does not list the digest in lowercase with a count:\n%s", vi)
	}
	if !strings.Contains(vi, "[entities](entities/index.md)") {
		t.Errorf("views index does not list the hub directories:\n%s", vi)
	}
}

func TestBuild_EmptyProducesMinimalValidBundle(t *testing.T) {
	b, _ := Build(RepoIdentity{ID: "x"}, nil, nil, RenderOpts{})
	m := bundleMap(b)
	if _, ok := m["index.md"]; !ok {
		t.Error("empty bundle must still have root index.md")
	}
	// No log.md: an empty changelog is not a document, matching renderRetired
	// and the digests, which already decline to write one.
	if _, ok := m["log.md"]; ok {
		t.Error("an empty bundle must not carry a log.md saying nothing happened")
	}
}

// TestOKFType_UnknownTopicIsVerbatim pins the singularization rule. English has
// no reliable singularization rule: a strip-trailing-"s" fallback turned the
// topic "business" into "busines" on a knowledge base using a non-code
// ontology. Known topics use the explicit table; everything else is verbatim.
// TestBuild_CitersAreSorted pins determinism: citersByPath is built by ranging
// facts, so without an explicit sort the order of a multi-citer list varies.
func TestBuild_CitersAreSorted(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	target := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf]
---
# Target fact

Body.`, ts)
	mk := func(uuid, title string) FactInput {
		return factInput(t, "kb/invariants/okf/c"+uuid+"/"+uuid+".md", `---
kind: pragmatic
type: policy
domain: [okf]
refs: ["kb/decisions/okf/scope/d9d6557d.md"]
---
# `+title+`

Body.`, ts)
	}
	// Supplied deliberately out of alphabetical order.
	facts := []FactInput{target, mk("cccccccc", "Zulu cites it"), mk("aaaaaaaa", "Alpha cites it"), mk("bbbbbbbb", "Mike cites it")}

	doc := bundleMap(mustBuild(t, facts))["kb/decisions/okf/scope/target-fact-d9d6557d.md"]
	sec := doc[strings.Index(doc, "# Cited by"):]
	ai, mi, zi := strings.Index(sec, "Alpha"), strings.Index(sec, "Mike"), strings.Index(sec, "Zulu")
	if !(ai < mi && mi < zi) {
		t.Errorf("citers not sorted by title (Alpha<Mike<Zulu), got offsets %d/%d/%d:\n%s", ai, mi, zi, sec)
	}
}

// TestBuild_HubMembersAreSorted pins the same property for hub pages.
func TestBuild_HubMembersAreSorted(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	mk := func(uuid, title string) FactInput {
		return factInput(t, "kb/decisions/okf/d"+uuid+"/"+uuid+".md", `---
kind: epistemic
type: principle
domain: [shared]
---
# `+title+`

Body.`, ts)
	}
	facts := []FactInput{mk("cccccccc", "Zulu"), mk("aaaaaaaa", "Alpha"), mk("bbbbbbbb", "Mike")}
	hub := bundleMap(mustBuild(t, facts))["views/domains/shared.md"]
	ai, mi, zi := strings.Index(hub, "Alpha"), strings.Index(hub, "Mike"), strings.Index(hub, "Zulu")
	if !(ai < mi && mi < zi) {
		t.Errorf("hub members not sorted by title:\n%s", hub)
	}
}

// TestBuild_DigestIsNewestFirst pins the chronology every digest blurb claims.
// A single-fact fixture is ordered correctly under any comparator, so this uses
// three across distinct days.
func TestBuild_DigestIsNewestFirst(t *testing.T) {
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	mk := func(uuid, title string, days int) FactInput {
		return factInput(t, "kb/architecture/store/"+uuid+".md", `---
kind: epistemic
type: synthesis
domain: [store]
origin: distilled
---
# `+title+`

Body.`, base.AddDate(0, 0, days))
	}
	facts := []FactInput{mk("aaaaaaaa", "Oldest", 0), mk("bbbbbbbb", "Middle", 10), mk("cccccccc", "Newest", 20)}
	dig := bundleMap(mustBuild(t, facts))["views/synthesis.md"]
	body := dig[strings.Index(dig, "# Synthesis"):]
	n, m, o := strings.Index(body, "Newest"), strings.Index(body, "Middle"), strings.Index(body, "Oldest")
	if !(n < m && m < o) {
		t.Errorf("digest not newest-first, got offsets %d/%d/%d:\n%s", n, m, o, body)
	}
}

// TestConcept_StatusDraft pins the POSITIVE side of statusFor. The existing
// test only asserts status is absent for a confident fact, so a mutation making
// statusFor always return "" survives.
func TestConcept_StatusDraft(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	hyp := factInput(t, "kb/decisions/okf/h/aaaaaaaa.md", `---
kind: epistemic
type: hypothesis
domain: [okf]
confidence: 0.9
---
# A prediction

Body.`, ts)
	low := factInput(t, "kb/decisions/okf/l/bbbbbbbb.md", `---
kind: epistemic
type: observation
domain: [okf]
confidence: 0.3
---
# A shaky claim

Body.`, ts)
	m := bundleMap(mustBuild(t, []FactInput{hyp, low}))
	for path, why := range map[string]string{
		"kb/decisions/okf/h/a-prediction-aaaaaaaa.md":  "a hypothesis is provisional by construction",
		"kb/decisions/okf/l/a-shaky-claim-bbbbbbbb.md": "confidence 0.3 is below the draft threshold",
	} {
		if !strings.Contains(m[path], "status: draft") {
			t.Errorf("%s: expected status: draft (%s):\n%s", path, why, m[path])
		}
	}
}

// mustBuild is a helper for the tests above.
func mustBuild(t *testing.T, facts []FactInput) Bundle {
	t.Helper()
	b, skips := Build(RepoIdentity{ID: "x"}, facts, nil, RenderOpts{})
	if skips.Skipped != 0 {
		t.Fatalf("unexpected skips: %+v", skips)
	}
	return b
}

func TestOKFType_UnknownTopicIsVerbatim(t *testing.T) {
	cases := map[string]string{
		"kb/business/ai/x/aa11bb22.md":    "business",
		"kb/physics/quantum/aa11bb22.md":  "physics",
		"kb/technology/ai/aa11bb22.md":    "technology",
		"kb/decisions/okf/aa11bb22.md":    "decision",  // known: singularized
		"kb/invariants/okf/aa11bb22.md":   "invariant", // known: singularized
		"kb/architecture/okf/aa11bb22.md": "architecture",
	}
	for path, want := range cases {
		if got := okfType(path, "observation"); got != want {
			t.Errorf("okfType(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestBuild_HubKeyNamedIndexDoesNotClaimTheDirectoryIndex pins the reservation
// in assignPaths. A domain tag literally named "index" slugifies to "index.md",
// which renderHubs writes the DIRECTORY index to afterwards — so without the
// reservation the hub page was silently overwritten and every concept document
// in that group linked to the directory listing instead of its hub.
func TestBuild_HubKeyNamedIndexDoesNotClaimTheDirectoryIndex(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	var facts []FactInput
	for _, id := range []string{"aa11bb22", "cc33dd44"} {
		facts = append(facts, factInput(t, "kb/decisions/okf/scope/"+id+".md", `---
kind: epistemic
type: principle
domain: [index]
---
# Fact `+id+`

Body.`, ts))
	}

	b, _ := Build(RepoIdentity{ID: "x"}, facts, nil, RenderOpts{})
	m := bundleMap(b)

	hub, ok := m["views/domains/index-2.md"]
	if !ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			if strings.HasPrefix(k, "views/domains/") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		t.Fatalf("hub page for the domain \"index\" missing; views/domains holds:\n%s",
			strings.Join(keys, "\n"))
	}
	if !strings.Contains(hub, "knomit_hub_key:") {
		t.Errorf("views/domains/index-2.md is not a hub page:\n%s", hub)
	}
	// The directory index must still be the directory index.
	if idx := m["views/domains/index.md"]; !strings.HasPrefix(idx, "# Domains\n") {
		t.Errorf("directory index was overwritten by a hub page:\n%s", idx)
	}
	// And every concept document links to the hub, not to the listing.
	concept := m["kb/decisions/okf/scope/fact-aa11bb22-aa11bb22.md"]
	if !strings.Contains(concept, "views/domains/index-2.md") {
		t.Errorf("concept does not link to the hub page:\n%s", concept)
	}
}

// TestBuild_RepeatedRefIsPresentedOnceButRoundTripsVerbatim pins both halves of
// the ref dedup: what a reader SEES is deduplicated, what the importer reads is
// not.
func TestBuild_RepeatedRefIsPresentedOnceButRoundTripsVerbatim(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	f := factInput(t, "kb/decisions/okf/scope/aa11bb22.md", `---
kind: epistemic
type: principle
domain: [okf]
refs: ["https://example.com/a", "https://example.com/a", "https://example.com/b"]
---
# Repeated ref

Body.`, ts)

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{f}, nil, RenderOpts{})
	doc := bundleMap(b)["kb/decisions/okf/scope/repeated-ref-aa11bb22.md"]
	if doc == "" {
		t.Fatal("concept document missing")
	}
	if n := strings.Count(doc, "- resource: https://example.com/a"); n != 1 {
		t.Errorf("sources[] carries the repeated ref %d times, want 1:\n%s", n, doc)
	}
	if n := strings.Count(doc, "- [https://example.com/a](https://example.com/a)"); n != 1 {
		t.Errorf("Citations carries the repeated ref %d times, want 1:\n%s", n, doc)
	}
	// Order is first-seen, never sorted: b must still follow a.
	if strings.Index(doc, "example.com/b") < strings.Index(doc, "example.com/a") {
		t.Error("dedup reordered the authored refs")
	}
	// knomit_refs is the lossless channel and keeps the repeat.
	got, err := ParseConcept([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.com/a", "https://example.com/a", "https://example.com/b"}
	if !reflect.DeepEqual(got.Refs, want) {
		t.Errorf("round-tripped refs: got %v want %v", got.Refs, want)
	}
}
