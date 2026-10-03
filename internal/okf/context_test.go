package okf

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// F22: a fact's context is exported as a knomit_context frontmatter map and a
// Context line in Related, and ParseConcept reads the same map back — values
// typed as they were, including strings that look like other types.
//
// SABOTAGE: drop KnomitContext from the round-trip struct → the map is lost
// on re-import → red.
func TestConcept_ContextExportAndRoundTrip(t *testing.T) {
	content := `---
type: observation
domain: [verdicts]
confidence: 0.8
sources: 1
entities: [Verdict]
refs: []
context: {due: "2026-10-01", final: true, flag: "true", score: 0.5, task: t-17, verdict: disagree}
---
# A verdict

The verdict body.`
	orig := mkFact(t, "kb/verdicts/t-17/9a1b2c3d.md", content)
	require := func(cond bool, format string, a ...any) {
		t.Helper()
		if !cond {
			t.Fatalf(format, a...)
		}
	}
	require(len(orig.Context) == 6, "fixture: the context must parse: %v", orig.Context)

	doc, err := Concept(FactInput{Fact: orig, Timestamp: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}, RepoIdentity{ID: "x"}, "", RenderOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	require(strings.Contains(s, "knomit_context:"), "frontmatter carries knomit_context:\n%s", s)
	require(strings.Contains(s, "**Context:** due = 2026-10-01 · final = true · flag = true · score = 0.5 · task = t-17 · verdict = disagree\n"),
		"the Related section carries one Context line, sorted:\n%s", s)

	got, err := ParseConcept(doc)
	if err != nil {
		t.Fatal(err)
	}
	require(reflect.DeepEqual(got.Context, orig.Context), "round trip: got %#v want %#v", got.Context, orig.Context)
	require(got.Body == orig.Body, "the Context line is generated, not authored: body %q", got.Body)
}

// A fact without context exports no knomit_context and no Context line.
func TestConcept_NoContextNoKey(t *testing.T) {
	orig := mkFact(t, "kb/verdicts/a.md", "---\ntype: observation\ndomain: [v]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# Plain\n\nbody")
	doc, err := Concept(FactInput{Fact: orig, Timestamp: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}, RepoIdentity{ID: "x"}, "", RenderOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "context") || strings.Contains(string(doc), "Context") {
		t.Fatalf("no context, no key and no line:\n%s", doc)
	}
}
