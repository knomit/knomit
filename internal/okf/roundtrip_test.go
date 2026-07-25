// internal/okf/roundtrip_test.go
package okf

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip_ConceptReconstructsFact(t *testing.T) {
	content := `---
kind: pragmatic
type: policy
domain: [okf]
confidence: 0.9
entities: [refs]
sources: 2
refs: ["internal/store/remote_sync.go:238"]
evidence_weight: 3
origin: authored
---
# Refs never pushed

Generated okf/* refs must never reach any remote.`
	orig := mkFact(t, "kb/invariants/okf/refs-never-pushed/3209d651.md", content)
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

	doc, err := Concept(FactInput{Fact: orig, Timestamp: ts}, RepoIdentity{ID: "x"}, "", RenderOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseConcept(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path() != orig.Path() {
		t.Errorf("path: got %q want %q", got.Path(), orig.Path())
	}
	if got.Type != orig.Type || got.Kind != orig.Kind {
		t.Errorf("core scalar fields lost: got type=%q kind=%q", got.Type, got.Kind)
	}
	if !reflect.DeepEqual(got.Domain, orig.Domain) {
		t.Errorf("domain lost: got %v want %v", got.Domain, orig.Domain)
	}
	if !reflect.DeepEqual(got.Entities, orig.Entities) {
		t.Errorf("entities lost: got %v want %v", got.Entities, orig.Entities)
	}
	if !reflect.DeepEqual(got.Refs, orig.Refs) {
		t.Errorf("refs lost: got %v want %v", got.Refs, orig.Refs)
	}
	if got.Sources != orig.Sources || got.EvidenceWeight != orig.EvidenceWeight || got.Origin != orig.Origin {
		t.Errorf("provenance fields lost: sources=%d evidence=%g origin=%q",
			got.Sources, got.EvidenceWeight, got.Origin)
	}
	if got.Title != orig.Title {
		t.Errorf("title: got %q want %q", got.Title, orig.Title)
	}
}

// TestRoundTrip_BodyIsExactlyTheAuthoredBody pins the property the round-trip
// exists to guarantee. The generated sections are export artifacts; none of
// them may survive back into a fact's body.
func TestRoundTrip_BodyIsExactlyTheAuthoredBody(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	const authored = "The authored body text."
	orig := mkFact(t, "kb/invariants/okf/x/3209d651.md", `---
kind: pragmatic
type: policy
domain: [okf]
entities: [SwapStore]
refs: ["https://example.com/x"]
---
# Refs never pushed

`+authored)

	doc, err := Concept(FactInput{Fact: orig, Timestamp: ts}, RepoIdentity{ID: "x"},
		"kb/invariants/okf/x", RenderOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseConcept(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != authored {
		t.Errorf("body not preserved:\n got: %q\nwant: %q", got.Body, authored)
	}
	for _, h := range []string{"# Related", "# Citations", "# Cited by", "# History"} {
		if strings.Contains(got.Body, h) {
			t.Errorf("generated section %q leaked into the body:\n%s", h, got.Body)
		}
	}
}
