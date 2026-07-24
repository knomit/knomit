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
	// A leaf directory's index lists the concept by title + type.
	leaf := m["decisions/okf/scope/index.md"]
	if !strings.Contains(leaf, "Export scope is repo only") || !strings.Contains(leaf, "decision") {
		t.Errorf("leaf index missing concept entry:\n%s", leaf)
	}
	// A non-root index.md must NOT carry frontmatter (okf_version only at root).
	if strings.Contains(leaf, "okf_version") {
		t.Errorf("non-root index must not carry okf_version:\n%s", leaf)
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
