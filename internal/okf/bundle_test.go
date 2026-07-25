// internal/okf/bundle_test.go
package okf

import (
	"sort"
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

	// An internal fact edge resolves to the TARGET's bundle document, relative.
	wantInternal := "](../../../decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md)"
	if !strings.Contains(doc, wantInternal) {
		t.Errorf("internal fact edge not linked (want %q):\n%s", wantInternal, doc)
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

func TestBuild_EmptyProducesMinimalValidBundle(t *testing.T) {
	b, _ := Build(RepoIdentity{ID: "x"}, nil, nil, RenderOpts{})
	m := bundleMap(b)
	if _, ok := m["index.md"]; !ok {
		t.Error("empty bundle must still have root index.md")
	}
	if _, ok := m["log.md"]; !ok {
		t.Error("empty bundle must still have log.md")
	}
}
