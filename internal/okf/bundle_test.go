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
		[]LogEntry{{Date: ts, Kind: "Creation", Title: "Export scope is repo only", Path: "kb/decisions/okf/scope/d9d6557d.md"}})
	if skips.Skipped != 0 {
		t.Fatalf("unexpected skips: %+v", skips)
	}
	m := bundleMap(b)

	// Concept doc paths: kb/ dropped, directories preserved, slug+uuid filename.
	if _, ok := m["decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md"]; !ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("concept path missing; have:\n%s", strings.Join(keys, "\n"))
	}
	if _, ok := m["invariants/okf/refs-never-pushed/refs-never-pushed-3209d651.md"]; !ok {
		t.Fatal("second concept path missing")
	}

	// Reserved root files.
	root, ok := m["index.md"]
	if !ok || !strings.Contains(root, `okf_version: "0.1"`) {
		t.Fatalf("root index.md missing okf_version:\n%s", root)
	}
	if _, ok := m["log.md"]; !ok {
		t.Fatal("log.md missing")
	}

	// Per-directory index.md exists at each level and lists children.
	for _, p := range []string{
		"decisions/index.md",
		"decisions/okf/index.md",
		"decisions/okf/scope/index.md",
		"invariants/index.md",
	} {
		if _, ok := m[p]; !ok {
			t.Errorf("missing index: %s", p)
		}
	}
	// A leaf directory's index must LINK the concept, not just name it —
	// index.md is OKF's progressive-disclosure surface, so a consumer (or a
	// human browsing on GitHub) has to be able to reach the document from it.
	leaf := m["decisions/okf/scope/index.md"]
	wantEntry := "- [Export scope is repo only](/decisions/okf/scope/export-scope-is-repo-only-d9d6557d.md) — decision\n"
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

	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{f}, nil)
	idx := bundleMap(b)["invariants/store/edges/index.md"]
	if !strings.Contains(idx, `\[:DERIVED_FROM\]`) {
		t.Errorf("brackets in title must be escaped in the link label:\n%s", idx)
	}
	// The link target must still be intact and reachable.
	if !strings.Contains(idx, "](/invariants/store/edges/") {
		t.Errorf("link target malformed:\n%s", idx)
	}
}

func TestBuild_EmptyProducesMinimalValidBundle(t *testing.T) {
	b, _ := Build(RepoIdentity{ID: "x"}, nil, nil)
	m := bundleMap(b)
	if _, ok := m["index.md"]; !ok {
		t.Error("empty bundle must still have root index.md")
	}
	if _, ok := m["log.md"]; !ok {
		t.Error("empty bundle must still have log.md")
	}
}
