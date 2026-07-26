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

// TestRoundTrip_AuthoredHeadingNamedLikeAGeneratedOne pins the peel-from-the-end
// rule in stripGenerated. A fact whose authored body contains "# Citations" —
// here inside a fenced code block, the shape a fact ABOUT this exporter would
// naturally take — must round-trip whole. Cutting at the first match anywhere
// silently truncated it, and nothing said so.
func TestRoundTrip_AuthoredHeadingNamedLikeAGeneratedOne(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	const authored = "Concept emits the sections in a fixed order:\n\n" +
		"```\n# Related\n# Citations\n# Cited by\n# History\n```\n\n" +
		"# Notes\n\nEverything after the body is generated."
	orig := mkFact(t, "kb/invariants/okf/x/3209d651.md", `---
kind: pragmatic
type: policy
domain: [okf]
entities: [Concept]
refs: ["https://example.com/x"]
---
# Generated sections

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
		t.Errorf("authored body truncated at a heading that only LOOKS generated:\n got: %q\nwant: %q",
			got.Body, authored)
	}
}

// TestStripGenerated_PeelsOnlyTheTrailingSections covers the boundaries directly,
// including the ambiguity the doc comment admits to.
func TestStripGenerated_PeelsOnlyTheTrailingSections(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"no generated sections", "\nText.\n", "\nText.\n"},
		{
			name: "all four peeled",
			body: "\nText.\n\n# Related\n\n**Domains:** a\n\n# Citations\n\n- x\n\n# Cited by\n\n- y\n\n# History\n\n- z\n",
			want: "\nText.\n",
		},
		{
			name: "authored heading above a real generated block survives",
			body: "\nText.\n\n# Cited by\n\nProse, then a subsection.\n\n## Detail\n\nMore.\n\n# Citations\n\n- x\n",
			want: "\nText.\n\n# Cited by\n\nProse, then a subsection.\n\n## Detail\n\nMore.\n",
		},
		{
			name: "heading inside a fence is not a section",
			body: "\nText.\n\n```\n# History\n```\n\n# Citations\n\n- x\n",
			want: "\nText.\n\n```\n# History\n```\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripGenerated(tc.body); got != tc.want {
				t.Errorf("stripGenerated:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
