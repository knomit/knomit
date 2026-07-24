// internal/okf/concept_test.go
package okf

import (
	"strings"
	"testing"
	"time"

	"knomit/internal/fact"
)

func mkFact(t *testing.T, path, content string) fact.Fact {
	t.Helper()
	f, err := fact.ParseFact(path, content)
	if err != nil {
		t.Fatalf("ParseFact: %v", err)
	}
	return f
}

func TestConcept_FrontmatterAndBody(t *testing.T) {
	// Leaf type is `policy` (a valid pragmatic leaf type); the OKF `type` output
	// is derived from the TOPIC (`invariants` → `invariant`), and the leaf is
	// preserved as knomit_type.
	content := `---
kind: pragmatic
type: policy
domain: [okf]
confidence: 0.9
entities: [refs, push]
sources: 2
refs: ["internal/store/remote_sync.go:238"]
evidence_weight: 3
origin: authored
---
# Refs never pushed

Generated okf/* refs must never reach any remote.`
	f := mkFact(t, "kb/invariants/okf/refs-never-pushed/3209d651.md", content)
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

	out, err := Concept(FactInput{Fact: f, Timestamp: ts}, RepoIdentity{ID: "0123456789ab"})
	if err != nil {
		t.Fatalf("Concept: %v", err)
	}
	s := string(out)

	wantContains := []string{
		"type: invariant\n", // OKF type = singularized topic, NOT the leaf type
		"title: Refs never pushed\n",
		"resource: knomit://0123456789ab/kb/invariants/okf/refs-never-pushed/3209d651.md\n",
		"timestamp: \"2026-07-22T10:00:00Z\"\n",
		"knomit_type: policy\n", // leaf type preserved for round-trip
		"knomit_kind: pragmatic\n",
		"knomit_sources: 2\n",
		"knomit_path: kb/invariants/okf/refs-never-pushed/3209d651.md\n",
		"# Refs never pushed",
		"# Citations",
		"- `internal/store/remote_sync.go:238`",
	}
	for _, w := range wantContains {
		if !strings.Contains(s, w) {
			t.Errorf("output missing %q\n---\n%s", w, s)
		}
	}
	if strings.Contains(s, "description:") {
		t.Errorf("description must be omitted, got:\n%s", s)
	}
	// tags = domain + entities + kind, in that order.
	if !strings.Contains(s, "tags:") ||
		!strings.Contains(s, "- okf") ||
		!strings.Contains(s, "- refs") ||
		!strings.Contains(s, "- pragmatic") {
		t.Errorf("tags mapping wrong:\n%s", s)
	}
}

func TestConcept_NoTypeIsError(t *testing.T) {
	// A fact with empty type must be rejected so it is never emitted
	// non-conformant. ParseFact normally guards this; the check exists so the
	// path stays dead.
	f := fact.Fact{} // zero value: empty Type
	_, err := Concept(FactInput{Fact: f}, RepoIdentity{ID: "x"})
	if err == nil {
		t.Fatal("expected error for empty type, got nil")
	}
}
