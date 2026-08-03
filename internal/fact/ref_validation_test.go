package fact

import (
	"strings"
	"testing"
)

func TestValidateRefs(t *testing.T) {
	ok := []string{
		"kb/decisions/x/abc.md",
		"kb://3ec012f5b4d2/kb/decisions/x/abc.md",
		"https://github.com/knomit/knomit/discussions/8",
		"file:///Users/pba/notes.md",
		"src://knomit/internal/store/repo.go@ca1c272", // legacy: accepted forever
		"src://knomit/internal/repos/auth.go",         // legacy: no version at all
		"src://7b4887ce51d9/internal/store/git/storer.go@" + testCommit40 + ":" + testBlob40,
		"src://7b4887ce51d9/internal/repos/instance.go@" + testCommit40 + ":" + testBlob40 + "#L241-L259",
	}
	for _, ref := range ok {
		if err := ValidateRefs([]string{ref}); err != nil {
			t.Errorf("ValidateRefs(%q) = %v, want nil", ref, err)
		}
	}
	// Nil and empty slices are fine — ParseFact coerces absent refs to []string{}.
	if err := ValidateRefs(nil); err != nil {
		t.Errorf("ValidateRefs(nil) = %v, want nil", err)
	}

	bad := []struct{ ref, wantSubstr string }{
		{"kb://abc/kb/x.md", "malformed kb://"},
		{"src://7b4887ce51d9/", "malformed src://"},
		{"", "empty ref"},
		// New form (12-hex id AND a blob) must carry FULL hashes.
		{"src://7b4887ce51d9/x.go@ca1c272:36b1d451", "40-hex"},
		{"src://7b4887ce51d9/x.go@" + testCommit40 + ":36b1d451", "40-hex"},
		{"src://7b4887ce51d9/x.go@ca1c272:" + testBlob40, "40-hex"},
	}
	for _, tc := range bad {
		err := ValidateRefs([]string{tc.ref})
		if err == nil {
			t.Errorf("ValidateRefs(%q) = nil, want error containing %q", tc.ref, tc.wantSubstr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubstr) {
			t.Errorf("ValidateRefs(%q) = %v, want error containing %q", tc.ref, err, tc.wantSubstr)
		}
	}
}

// The error is read by an agent that must fix the refs and retry, so it must
// report EVERY problem (not just the first) and say how to obtain the right
// value — one problem per round-trip is the difference between one retry and
// five. It must also state that the legacy src form is still accepted, or an
// agent hitting one bad ref will "helpfully" rewrite the legacy ones too.
func TestValidateRefs_ErrorIsActionable(t *testing.T) {
	err := ValidateRefs([]string{
		"kb://abc/x.md",
		"src://7b4887ce51d9/internal/x.go@ca1c272:36b1d451",
		"kb/fine.md",
	})
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()

	for _, want := range []string{
		"kb://abc/x.md",                        // first problem named
		"internal/x.go",                        // second problem named
		"got 7 chars",                          // says how far off the commit is
		"git rev-parse ca1c272",                // the remedy, with the real value
		"git rev-parse <commit>:internal/x.go", // the remedy for the blob
		"legacy source form, still accepted",   // do not rewrite legacy refs
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q\n--- got ---\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "kb/fine.md") {
		t.Errorf("valid ref should not appear in the error:\n%s", msg)
	}
}

// The ref axis is DELIBERATELY ASYMMETRIC, in exactly one direction, exactly
// like origin×type: SerializeFact refuses a malformed ref, ParseFact reads it
// and records a warning.
//
// The reason is the same one written into ParseFact for origin, and it is the
// historical-not-current principle: this is a historical graph, so a version
// that was legal when committed must stay readable forever. Failing the read
// deleted the fact from the search index and the provenance graph without a
// word — a rebuild does not fail on it — so one bad ref made a fact
// unviewable AND unrepairable.
//
// Only one direction of asymmetry is safe, and this pins both halves:
//
//   - ParseFact must accept EVERYTHING SerializeFact emits. The reverse
//     (readable but never writable back) is the failure the original symmetry
//     guard existed to catch, and it is still caught here.
//   - Where ParseFact accepts MORE, it must say so via RefWarnings. Silent
//     tolerance is how a corpus fills with citations nobody can follow.
func TestRefValidation_ParseIsLenientButNeverSilent(t *testing.T) {
	valid := []string{
		"kb/decisions/x/abc.md",
		"kb://3ec012f5b4d2/kb/decisions/x/abc.md",
		"https://example.com/x",
		"file:///tmp/x",
		"src://knomit/internal/store/repo.go@ca1c272",
		"src://7b4887ce51d9/internal/x.go@" + testCommit40 + ":" + testBlob40,
	}
	malformed := []string{
		"kb://abc/kb/x.md",                        // bad repo id
		"src://7b4887ce51d9/",                     // no path
		"src://7b4887ce51d9/x.go@ca1c272:36b1d45", // new form, abbreviated hashes
	}

	factWith := func(ref string) Fact {
		f := NewFact("kb/decisions/x/sym.md")
		f.Title, f.Body, f.Type = "t", "b", Observation
		f.Confidence, f.Sources = 0.5, 1
		f.Refs = []string{ref}
		return f
	}
	docWith := func(ref string) string {
		return "---\ntype: observation\nconfidence: 0.5\nsources: 1\nrefs: ['" +
			ref + "']\n---\n# t\n\nb\n"
	}

	// Everything writable must be readable back, warning-free.
	for _, ref := range valid {
		out, serErr := SerializeFact(factWith(ref))
		if serErr != nil {
			t.Errorf("%q: SerializeFact must accept a well-formed ref: %v", ref, serErr)
			continue
		}
		parsed, parseErr := ParseFact("kb/decisions/x/sym.md", out)
		if parseErr != nil {
			t.Errorf("ASYMMETRY for %q: SerializeFact emits it but ParseFact rejects it: %v", ref, parseErr)
			continue
		}
		if len(parsed.RefWarnings) != 0 {
			t.Errorf("%q: a well-formed ref must produce no warning, got %v", ref, parsed.RefWarnings)
		}
	}

	// Everything malformed is refused on write, read on load, and reported.
	for _, ref := range malformed {
		if _, serErr := SerializeFact(factWith(ref)); serErr == nil {
			t.Errorf("%q: SerializeFact must refuse a malformed ref — the write side is what "+
				"keeps the corpus clean", ref)
		}
		parsed, parseErr := ParseFact("kb/decisions/x/sym.md", docWith(ref))
		if parseErr != nil {
			t.Errorf("%q: ParseFact must READ a fact carrying a malformed ref — failing the read "+
				"deletes it from the index and the graph, silently: %v", ref, parseErr)
			continue
		}
		if len(parsed.RefWarnings) == 0 {
			t.Errorf("%q: leniency must never be silent — ParseFact must record a RefWarning", ref)
		}
		if len(parsed.Refs) != 1 || parsed.Refs[0] != ref {
			t.Errorf("%q: the ref itself must survive verbatim, got %v", ref, parsed.Refs)
		}
	}
}

// RefWarnings is derived on read, never stored: it must not reach the
// frontmatter, or a round trip would grow a field the format does not define.
func TestRefValidation_WarningsAreNotSerialized(t *testing.T) {
	f := NewFact("kb/decisions/x/sym.md")
	f.Title, f.Body, f.Type = "t", "b", Observation
	f.Confidence, f.Sources = 0.5, 1
	f.Refs = []string{"kb/decisions/x/abc.md"}
	f.RefWarnings = []string{"this must not be written"}

	out, err := SerializeFact(f)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if strings.Contains(out, "ref_warnings") || strings.Contains(out, "must not be written") {
		t.Errorf("RefWarnings leaked into the stored bytes:\n%s", out)
	}
}
