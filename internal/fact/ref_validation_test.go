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

// SYMMETRY: the set SerializeFact emits must equal the set ParseFact accepts.
// A shape rejected by one and accepted by the other produces a file that can be
// read but never written back, or the reverse. This is the guard whose absence
// let the origin×type asymmetry live for months.
func TestRefValidation_ParseSerializeSymmetry(t *testing.T) {
	for _, ref := range []string{
		"kb/decisions/x/abc.md",
		"kb://3ec012f5b4d2/kb/decisions/x/abc.md",
		"https://example.com/x",
		"file:///tmp/x",
		"src://knomit/internal/store/repo.go@ca1c272",
		"src://7b4887ce51d9/internal/x.go@" + testCommit40 + ":" + testBlob40,
		"kb://abc/kb/x.md",                        // malformed
		"src://7b4887ce51d9/",                     // malformed
		"src://7b4887ce51d9/x.go@ca1c272:36b1d45", // new form, abbreviated
	} {
		f := NewFact("kb/decisions/x/sym.md")
		f.Title = "t"
		f.Body = "b"
		f.Confidence = 0.5
		f.Sources = 1
		f.Type = Observation
		f.Refs = []string{ref}

		out, serErr := SerializeFact(f)
		if serErr != nil {
			// Rejected on write. ParseFact must reject the same shape, so build
			// a file carrying it directly and confirm.
			doc := "---\ntype: observation\nconfidence: 0.5\nsources: 1\nrefs: ['" +
				ref + "']\n---\n# t\n\nb\n"
			if _, parseErr := ParseFact("kb/decisions/x/sym.md", doc); parseErr == nil {
				t.Errorf("ASYMMETRY for %q: SerializeFact rejects it but ParseFact accepts it", ref)
			}
			continue
		}
		if _, parseErr := ParseFact("kb/decisions/x/sym.md", out); parseErr != nil {
			t.Errorf("ASYMMETRY for %q: SerializeFact emits it but ParseFact rejects it: %v", ref, parseErr)
		}
	}
}
