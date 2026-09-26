package fact

import (
	"strings"
	"testing"
)

// testSignerKey is a syntactically valid ssh-ed25519 authorized-key line. It
// is only parsed, never used to verify anything.
const testSignerKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl mindev.local-8ef0cd32"

const rootAttrYAML = `id: x
name: X
attributes:
  verify_signatures: enforce
  verify_signers:
    - ` + testSignerKey + `
topics:
  notes:
    description: d
`

// TestRootAttributes_Parse: the root block decodes into Ontology.Attributes
// and produces no diagnostic at all.
func TestRootAttributes_Parse(t *testing.T) {
	o, diags := ValidateOntologyYAML([]byte(rootAttrYAML))
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", diags)
	}
	if got := o.Attributes["verify_signatures"]; got != "enforce" {
		t.Fatalf("verify_signatures = %v, want enforce", got)
	}
	signers, ok := o.Attributes["verify_signers"].([]any)
	if !ok || len(signers) != 1 || signers[0] != testSignerKey {
		t.Fatalf("verify_signers = %#v, want [%q]", o.Attributes["verify_signers"], testSignerKey)
	}
}

// TestRootAttributes_SerializeRoundTrip: Serialize must emit the root block.
// Before this seam it dropped it, which is what let the boot refresh erase it.
func TestRootAttributes_SerializeRoundTrip(t *testing.T) {
	o := mustParse(t, rootAttrYAML)
	out, err := o.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "verify_signatures: enforce") {
		t.Fatalf("serialized ontology lost the root block:\n%s", out)
	}
	back := mustParse(t, string(out))
	if back.Attributes["verify_signatures"] != "enforce" {
		t.Fatalf("round trip lost verify_signatures: %#v", back.Attributes)
	}
	again, err := back.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Fatalf("serialize is not byte-stable:\n%s\n---\n%s", out, again)
	}
}

// TestRootAttributes_NoBlockSerializesAsBefore: a repo without a root block
// must serialize with no `attributes` key at the root (no behaviour change).
func TestRootAttributes_NoBlockSerializesAsBefore(t *testing.T) {
	o := mustParse(t, "id: x\nname: X\ntopics:\n  notes:\n    description: d\n")
	out, err := o.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if want := "id: x\nname: X\ntopics:\n  notes:\n    description: d\n"; string(out) != want {
		t.Fatalf("serialize changed for an ontology without root attributes:\n%q\nwant\n%q", out, want)
	}
}

// TestRootAttributes_AreDivergence: the refresh reads "subset" as safe to
// overwrite, so a root attribute the preset does not carry must be divergence,
// exactly like a topic attribute. A value that behaves as absent
// (verify_signatures: off) is compared as absent.
func TestRootAttributes_AreDivergence(t *testing.T) {
	preset := mustParse(t, "id: x\nname: X\ntopics:\n  notes:\n    description: d\n  more:\n    description: d\n")
	stored := mustParse(t, rootAttrYAML)
	if got := stored.SubsetDivergence(preset); got != DivergenceAttributes {
		t.Fatalf("SubsetDivergence = %q, want %q", got, DivergenceAttributes)
	}
	off := mustParse(t, "id: x\nname: X\nattributes:\n  verify_signatures: off\ntopics:\n  notes:\n    description: d\n")
	if got := off.SubsetDivergence(preset); got != "" {
		t.Fatalf("explicit off must compare as absent: SubsetDivergence = %q, want \"\"", got)
	}
	unknown := mustParse(t, "id: x\nname: X\nattributes:\n  from_the_future: 1\ntopics:\n  notes:\n    description: d\n")
	if got := unknown.SubsetDivergence(preset); got != DivergenceAttributes {
		t.Fatalf("an unknown root key must be preserved as divergence: got %q", got)
	}
}

// TestRootAttributes_ProblemsAreFatalOnlyForANewOntology: a key in the wrong
// scope, or a bad value for a root key, must never stop an EXISTING repo from
// opening (a parse failure on the open path refuses every write), but must be
// refused when an ontology is being created.
func TestRootAttributes_ProblemsAreFatalOnlyForANewOntology(t *testing.T) {
	cases := map[string]string{
		"root key on a topic": "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      verify_signatures: log\n",
		"topic key at root":   "id: x\nname: X\nattributes:\n  learn_dedup: off\ntopics:\n  notes:\n    description: d\n",
		"bad mode value":      "id: x\nname: X\nattributes:\n  verify_signatures: yes\ntopics:\n  notes:\n    description: d\n",
		"bad signer line":     "id: x\nname: X\nattributes:\n  verify_signers:\n    - not-a-key\ntopics:\n  notes:\n    description: d\n",
		"signers not a list":  "id: x\nname: X\nattributes:\n  verify_signers: " + testSignerKey + "\ntopics:\n  notes:\n    description: d\n",
		"rsa signer":          "id: x\nname: X\nattributes:\n  verify_signers:\n    - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7 x\ntopics:\n  notes:\n    description: d\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			o, err := ParseOntology([]byte(src))
			if err != nil || o == nil {
				t.Fatalf("open path must still parse: %v", err)
			}
			_, diags := ValidateOntologyYAML([]byte(src))
			if len(diags) == 0 {
				t.Fatal("the problem must be reported as a diagnostic")
			}
			if _, err := ParseNewOntology([]byte(src)); err == nil {
				t.Fatal("a NEW ontology with this problem must be refused")
			}
		})
	}
	if _, err := ParseNewOntology([]byte(rootAttrYAML)); err != nil {
		t.Fatalf("a valid root block must be accepted for a new ontology: %v", err)
	}
}

// TestRootAttributes_LearnDedupStaysFatalOnTopics: the existing rule for a bad
// value of a topic key is unchanged.
func TestRootAttributes_LearnDedupStaysFatalOnTopics(t *testing.T) {
	src := "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      learn_dedup: maybe\n"
	if _, err := ParseOntology([]byte(src)); err == nil {
		t.Fatal("a bad learn_dedup value must stay fatal")
	}
}
