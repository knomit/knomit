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
