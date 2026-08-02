package mcp

import (
	"context"
	"strings"
	"testing"

	"knomit/internal/store"
)

const gateRepoID = "3ec012f5b4d2"

// fakeFactIndex answers FactExists from a fixed set. Only FactExists is
// exercised; embedding the interface leaves every other method nil so an
// accidental new dependency panics loudly instead of passing silently.
type fakeFactIndex struct {
	store.FactIndex
	existing map[string]bool
}

func (f *fakeFactIndex) FactExists(_ context.Context, _, path string) (bool, error) {
	return f.existing[strings.ToLower(path)], nil
}

func TestRefGate_AcceptsExistingTarget(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{"kb/decisions/x/abc.md": true}}
	batch := map[string][]string{
		"kb/gotchas/y/new.md": {"kb/decisions/x/abc.md"},
	}
	if err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, nil); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

// The case that makes the gate ergonomic rather than a burden: three facts
// written in ONE call, all citing each other, none existing yet. All must be
// accepted, because BatchWriteFacts commits them together.
func TestRefGate_AcceptsIntraBatchCrossReferences(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{}}
	batch := map[string][]string{
		"kb/a.md": {"kb/b.md", "kb/c.md"},
		"kb/b.md": {"kb/c.md"},
		"kb/c.md": {"kb/a.md"}, // circular — still fine, one commit
	}
	if err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, nil); err != nil {
		t.Fatalf("intra-batch cross-references must be accepted, got %v", err)
	}
}

func TestRefGate_RejectsMissingTarget(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{}}
	batch := map[string][]string{
		"kb/gotchas/y/new.md": {"kb/nope.md"},
	}
	err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, nil)
	if err == nil {
		t.Fatal("want an error naming the unresolvable ref")
	}
	for _, want := range []string{"kb/gotchas/y/new.md", "kb/nope.md", "nothing was written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q\n--- got ---\n%v", want, err)
		}
	}
}

// Every problem in one message: an agent writing twenty facts with three typos
// should need one retry, not three.
func TestRefGate_ReportsAllProblems(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{}}
	batch := map[string][]string{
		"kb/a.md": {"kb/nope1.md", "kb/nope2.md"},
		"kb/b.md": {"kb/nope3.md"},
	}
	err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"kb/nope1.md", "kb/nope2.md", "kb/nope3.md", "kb/a.md", "kb/b.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q\n--- got ---\n%v", want, err)
		}
	}
}

// Canonical form must behave exactly like the bare form.
func TestRefGate_AcceptsQualifiedSelfReference(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{"kb/decisions/x/abc.md": true}}
	batch := map[string][]string{
		"kb/y/new.md": {"kb://3ec012f5b4d2/kb/decisions/x/abc.md"},
	}
	if err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, nil); err != nil {
		t.Fatalf("qualified self-reference must be accepted, got %v", err)
	}
}

// Foreign, source, and external refs are never gated — knomit cannot check them.
func TestRefGate_IgnoresUncheckableKinds(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{}}
	batch := map[string][]string{
		"kb/y/new.md": {
			"kb://7b4887ce51d9/kb/somewhere/else.md",
			"src://7b4887ce51d9/internal/x.go@" +
				"4154e92c8ff333435fd00c442489e855e4c3331e:36b1d45187d6a2c6ad18d591142227ad2a02a66e",
			"src://knomit/internal/legacy.go@ca1c272",
			"https://example.com/x",
			"file:///tmp/x",
		},
	}
	if err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, nil); err != nil {
		t.Fatalf("uncheckable ref kinds must pass, got %v", err)
	}
}

// A fact retracted in the SAME call cannot satisfy a ref: the commit is atomic,
// so the target is gone the instant the write lands.
func TestRefGate_RejectsRefToSameCallDeletion(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{"kb/old.md": true}}
	batch := map[string][]string{"kb/new.md": {"kb/old.md"}}
	err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, []string{"kb/old.md"})
	if err == nil {
		t.Fatal("want an error: kb/old.md is being retracted in this same call")
	}
	if !strings.Contains(err.Error(), "kb/old.md") {
		t.Errorf("error must mention kb/old.md: %v", err)
	}
}

// learn passes the authoritative on-disk path, which preserves the configured
// ontology root verbatim — "KB/..." for an uppercase root — while ClassifyRef
// lowercases. The batch membership check must fold case or an intra-batch
// cross-reference under an uppercase root would be wrongly rejected.
func TestRefGate_BatchMembershipIsCaseFolded(t *testing.T) {
	fi := &fakeFactIndex{existing: map[string]bool{}}
	batch := map[string][]string{
		"KB/decisions/x/One.md": {"KB/decisions/x/Two.md"},
		"KB/decisions/x/Two.md": {},
	}
	if err := checkLocalRefsResolve(context.Background(), fi, "agent", gateRepoID, batch, nil); err != nil {
		t.Fatalf("uppercase ontology root must not break batch membership: %v", err)
	}
}

// An author never needs to know a repo id: bare paths are qualified on write.
func TestCanonicalizeLocalRefs(t *testing.T) {
	const local = "3ec012f5b4d2"
	got := canonicalizeLocalRefs([]string{
		"kb/decisions/x/abc.md",              // bare → qualified
		"kb://3ec012f5b4d2/kb/y.md",          // already canonical → unchanged
		"kb/Decisions/X/Abc.md",              // canonicalized AND lowercased
		"kb://7b4887ce51d9/kb/z.md",          // another repo → untouched
		"src://knomit/internal/x.go@ca1c272", // source → untouched
		"https://example.com/a",              // external → untouched
		"file:///tmp/b",                      // external → untouched
	}, local)

	// "kb/decisions/x/abc.md" and "kb/Decisions/X/Abc.md" are the same fact, so
	// they collapse to one canonical ref rather than becoming a duplicate.
	want := []string{
		"kb://3ec012f5b4d2/kb/decisions/x/abc.md",
		"kb://3ec012f5b4d2/kb/y.md",
		"kb://7b4887ce51d9/kb/z.md",
		"src://knomit/internal/x.go@ca1c272",
		"https://example.com/a",
		"file:///tmp/b",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// With no resolvable identity, leave bare paths alone rather than qualifying
// them with an empty or wrong id.
func TestCanonicalizeLocalRefs_NoIDIsANoOp(t *testing.T) {
	in := []string{"kb/x.md", "https://e.com"}
	got := canonicalizeLocalRefs(in, "")
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("[%d] = %q, want %q unchanged", i, got[i], in[i])
		}
	}
}

// Canonicalizing must not MANUFACTURE a duplicate: a fact citing the same
// target once bare and once qualified holds two spellings of one edge, which
// collapse to a single ref.
func TestCanonicalizeLocalRefs_DedupesFormVariants(t *testing.T) {
	got := canonicalizeLocalRefs([]string{
		"kb/x.md",
		"kb://3ec012f5b4d2/kb/x.md",
		"kb/X.md",
		"https://e.com/a",
		"https://e.com/a",
	}, "3ec012f5b4d2")

	want := []string{"kb://3ec012f5b4d2/kb/x.md", "https://e.com/a"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
