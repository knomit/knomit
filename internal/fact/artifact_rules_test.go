package fact

import (
	"strings"
	"testing"
)

func ontWithArtifacts(section string) string {
	return "id: t\nname: T\nartifacts:\n" + section + "topics:\n  notes:\n    description: n\n"
}

// The fleet preset declares the PKI and the ledger rules (R7) and
// verify_signatures: enforce, and it is a valid NEW ontology.
func TestFleetPreset_DeclaresArtifactRulesAndEnforce(t *testing.T) {
	o := FleetOntology()
	if o.Attributes[AttrVerifySignatures] != "enforce" {
		t.Fatalf("verify_signatures = %v, want enforce", o.Attributes[AttrVerifySignatures])
	}
	raw, err := o.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNewOntology(raw); err != nil {
		t.Fatalf("the serialized preset is not a valid new ontology: %v", err)
	}
	l, ok := o.PKILayout()
	if !ok || l.Root != "artifacts/fleet/root.crt" || l.CRL != "artifacts/fleet/crl.pem" ||
		l.LeafPath("vic") != "artifacts/fleet/certs/vic.pem" {
		t.Fatalf("PKI layout %+v ok=%v", l, ok)
	}
	if id, ok := l.LeafID("vic.pem"); !ok || id != "vic" {
		t.Fatalf("LeafID(vic.pem) = %q, %v", id, ok)
	}
	for _, c := range []struct {
		path string
		want ArtifactAttrs
	}{
		{"artifacts/fleet/root.crt", ArtifactAttrs{Matched: true, PKI: PKIRoot, Change: ChangeWriteOnce, MaxSize: 16 << 10}},
		// The CRL grows with every revocation and never shrinks (the
		// superset rule): it carries no max_size.
		{"artifacts/fleet/crl.pem", ArtifactAttrs{Matched: true, PKI: PKICRL, Change: ChangeReplace, ReplaceWhen: ReplaceCRLNewer, RevokedSet: RevokedSetSuperset}},
		{"artifacts/fleet/certs/vic.pem", ArtifactAttrs{Matched: true, PKI: PKILeaf, Change: ChangeReplace, ReplaceWhen: ReplaceLeafRenewal, MaxSize: 16 << 10}},
		{"artifacts/fleet/issued/2026-10-14-x.pem", ArtifactAttrs{Matched: true, PKI: PKIIssuance, Change: ChangeWriteOnce, MaxSize: 16 << 10}},
		{"artifacts/fleet/revocations/2026-10-14-x.txt", ArtifactAttrs{Matched: true, PKI: PKIRevocation, Change: ChangeWriteOnce, MaxSize: 4 << 10}},
		// The ledgers are gone: no rule names them.
		{"artifacts/fleet/issued.jsonl", ArtifactAttrs{}},
		{"artifacts/fleet/revoked.jsonl", ArtifactAttrs{}},
		{"artifacts/fleet/notes/x.md", ArtifactAttrs{}},
		{"artifacts/fleet/certs/sub/x.pem", ArtifactAttrs{}}, // * never crosses '/'
		{"artifacts/other/x", ArtifactAttrs{}},
		{"kb/members/vic/1.md", ArtifactAttrs{}},
	} {
		got, err := o.ArtifactAttrs(c.path)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v %v, want %+v", c.path, got, err, c.want)
		}
	}
}

// Unknown key: a warning on the open path (kept by Serialize), fatal for a
// new ontology. Bad value, malformed pattern, inconsistent block: fatal on
// both paths.
func TestArtifactSection_ParseStrictness(t *testing.T) {
	unknown := ontWithArtifacts("  a/x: { change: write-once, chnage: append-only }\n")
	o, err := ParseOntology([]byte(unknown))
	if err != nil {
		t.Fatalf("open path refused an unknown key: %v", err)
	}
	if a, _ := o.ArtifactAttrs("artifacts/a/x"); a.Change != ChangeWriteOnce {
		t.Fatalf("the known key beside the unknown one was lost: %+v", a)
	}
	raw, _ := o.Serialize()
	if !strings.Contains(string(raw), "chnage") {
		t.Fatalf("the unknown key was not preserved:\n%s", raw)
	}
	if _, err := ParseNewOntology([]byte(unknown)); err == nil || !strings.Contains(err.Error(), `unknown key "chnage"`) {
		t.Fatalf("create path: err=%v, want the unknown key refused", err)
	}

	for name, section := range map[string]string{
		"bad change":                "  a/x: { change: append }\n",
		"bad pki":                   "  a/x: { pki: ca, change: write-once }\n",
		"bool value":                "  a/x: { change: true }\n",
		"bad size":                  "  a/**: { max_size: 16kb }\n",
		"zero size":                 "  a/**: { max_size: 0 }\n",
		"replace without condition": "  a/x: { change: replace }\n",
		"condition without replace": "  a/x: { change: write-once, replace_when: crl-newer }\n",
		"crl-newer on a non-crl":    "  a/x: { change: replace, replace_when: crl-newer }\n",
		"root not write-once":       "  a/r: { pki: root, change: append-only }\n",
		"crl without its rule":      "  a/r: { pki: root, change: write-once }\n  a/c: { pki: crl }\n",
		"wildcard root":             "  a/*.crt: { pki: root, change: write-once }\n",
		"leaf with two stars":       "  a/r: { pki: root, change: write-once }\n  a/*/*.pem: { pki: leaf, change: replace, replace_when: leaf-renewal }\n",
		"crl without root":          "  a/c: { pki: crl, change: replace, replace_when: crl-newer }\n",
		"two roots":                 "  a/r: { pki: root, change: write-once }\n  b/r: { pki: root, change: write-once }\n",
		"dot segment":               "  a/.hidden: { change: write-once }\n",
		"empty segment":             "  a//x: { change: write-once }\n",
		"partial double star":       "  a/**x: { change: write-once }\n",
		"block is not a mapping":    "  a/x: write-once\n",
		"revoked_set on a non-crl":  "  a/x: { change: write-once, revoked_set: superset }\n",
		"bad revoked_set":           "  a/r: { pki: root, change: write-once }\n  a/c: { pki: crl, change: replace, replace_when: crl-newer, revoked_set: subset }\n",
		"issuance not write-once":   "  a/r: { pki: root, change: write-once }\n  a/i/*.pem: { pki: issuance, change: append-only }\n",
		"revocation replaceable":    "  a/r: { pki: root, change: write-once }\n  a/v/*.txt: { pki: revocation, change: replace, replace_when: crl-newer }\n",
		"issuance without root":     "  a/i/*.pem: { pki: issuance, change: write-once }\n",
	} {
		doc := ontWithArtifacts(section)
		if _, err := ParseOntology([]byte(doc)); err == nil {
			t.Errorf("%s: the open path accepted it", name)
		}
		if _, err := ParseNewOntology([]byte(doc)); err == nil {
			t.Errorf("%s: the create path accepted it", name)
		}
	}
}

// How several matching patterns combine: max_size is the smallest of all
// matches; change (with replace_when) and pki come from the most specific
// match; two equally specific patterns that disagree are an error, never a
// guess.
func TestArtifactAttrs_Combination(t *testing.T) {
	o, err := ParseNewOntology([]byte(ontWithArtifacts(
		"  d/**: { max_size: 1MiB, change: append-only }\n" +
			"  d/*.log: { max_size: 100 }\n" +
			"  d/keep.log: { change: write-once }\n" +
			"  e/*.x: { change: write-once }\n" +
			"  e/a.*: { change: append-only }\n")))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path   string
		change string
		size   int64
	}{
		{"artifacts/d/keep.log", ChangeWriteOnce, 100},   // literal beats wildcard beats **; smallest cap
		{"artifacts/d/other.log", ChangeAppendOnly, 100}, // d/*.log sets no change: d/** supplies it
		{"artifacts/d/sub/x", ChangeAppendOnly, 1 << 20},
		{"artifacts/D/KEEP.LOG", ChangeWriteOnce, 100}, // ASCII case-insensitive, like trigger globs
	} {
		a, err := o.ArtifactAttrs(c.path)
		if err != nil || a.Change != c.change || a.MaxSize != c.size {
			t.Errorf("%s: %+v %v, want change %q size %d", c.path, a, err, c.change, c.size)
		}
	}
	if _, err := o.ArtifactAttrs("artifacts/e/a.x"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("equally specific, disagreeing patterns: err=%v, want ambiguous", err)
	}
	if a, err := o.ArtifactAttrs("artifacts/e/b.x"); err != nil || a.Change != ChangeWriteOnce {
		t.Fatalf("only one pattern matches: %+v %v", a, err)
	}
}

// The boot refresh may never add, change or drop an artifact rule: a stored
// rule the preset lacks is divergence, and so is a preset that adds one.
func TestArtifactRules_AreRefreshDivergence(t *testing.T) {
	with, err := ParseOntology([]byte(ontWithArtifacts("  a/x: { change: write-once }\n")))
	if err != nil {
		t.Fatal(err)
	}
	without, err := ParseOntology([]byte("id: t\nname: T\ntopics:\n  notes:\n    description: n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := with.SubsetDivergence(without); d != DivergenceAttributes {
		t.Fatalf("stored rule, preset without it: divergence %q, want %q", d, DivergenceAttributes)
	}
	if d := without.SubsetDivergence(with); d != "" {
		t.Fatalf("stored without, preset with: SubsetDivergence %q (the refresh guard is ArtifactRulesEqual)", d)
	}
	if ArtifactRulesEqual(without, with) || !ArtifactRulesEqual(with, with) || !ArtifactRulesEqual(without, without) {
		t.Fatal("ArtifactRulesEqual")
	}
}
