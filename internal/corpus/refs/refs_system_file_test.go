package refs

import (
	"context"
	"strings"
	"testing"
)

// filesIn is a file resolver over a fixed set, EXACT case (a system file is a
// real file in a case-sensitive tree, unlike a fact path).
func filesIn(paths ...string) ResolveFunc {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return func(_ context.Context, path string) (bool, error) { return set[path], nil }
}

// TestGate_SystemFileMustExist: a newly added .knomit/ ref whose file is not
// at the tip refuses the whole batch, echoes the ref verbatim, and carries the
// system-file fix line; one that exists passes, and so does a missing one the
// fact already carried (not re-judged).
// Sabotage: skip the RefLocalSystemFile branch in CheckBatch (fall through to
// the kind skip) → the missing file is accepted → red.
func TestGate_SystemFileMustExist(t *testing.T) {
	g := newGate().WithFiles(filesIn(".knomit/ontology.yaml"))
	ctx := context.Background()

	if err := g.CheckBatch(ctx, map[string][]string{"kb/a.md": {".knomit/ontology.yaml"}}, nil); err != nil {
		t.Fatalf("an existing file must resolve: %v", err)
	}

	err := g.CheckBatch(ctx, map[string][]string{
		"kb/a.md": {".knomit/Ontology.yaml", "kb://" + gateRepoID + "/.knomit/triggers/none.js"},
	}, nil)
	if err == nil {
		t.Fatal("a missing system file must refuse the write")
	}
	for _, want := range []string{
		"kb/a.md cites .knomit/Ontology.yaml, which does not exist",
		"kb/a.md cites kb://" + gateRepoID + "/.knomit/triggers/none.js, which does not exist",
		"does not exist under .knomit/ at the tip of the branch",
		"nothing was written",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q\n--- got ---\n%v", want, err)
		}
	}

	prior := map[string][]string{"kb/a.md": {"kb://" + gateRepoID + "/.knomit/gone.js"}}
	if err := g.CheckBatch(ctx, map[string][]string{"kb/a.md": {".knomit/gone.js"}}, prior); err != nil {
		t.Fatalf("a carried system-file ref must not be re-judged: %v", err)
	}
}

// TestGate_SystemFileNeedsResolver: a gate built without a file resolver
// refuses a newly added system-file ref instead of letting it through.
func TestGate_SystemFileNeedsResolver(t *testing.T) {
	err := newGate().CheckBatch(context.Background(), map[string][]string{"kb/a.md": {".knomit/x.js"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot check refs to .knomit/ files") {
		t.Fatalf("want the no-resolver refusal, got %v", err)
	}
}

// TestGate_SystemFileCanonicalized: a bare .knomit/ ref is stored as
// kb://<own-id>/.knomit/<path>, case kept; the canonical form of the same file
// collapses into it.
// Sabotage: leave RefLocalSystemFile out of Canonicalize → the bare ref is
// stored bare → red.
func TestGate_SystemFileCanonicalized(t *testing.T) {
	g := newGate().WithFiles(filesIn(".knomit/skills/x/SKILL.md"))
	out, changed, err := g.Apply(context.Background(), "kb/a.md",
		[]string{".knomit/skills/x/SKILL.md", "kb://" + gateRepoID + "/.knomit/skills/x/SKILL.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "kb://" + gateRepoID + "/.knomit/skills/x/SKILL.md"
	if !changed || len(out) != 1 || out[0] != want {
		t.Fatalf("got %v (changed=%v), want [%s]", out, changed, want)
	}
}

// TestGate_ForeignSystemFileUnchecked: a .knomit/ file in ANOTHER repo is
// never checked (like a foreign fact) and never rewritten.
func TestGate_ForeignSystemFileUnchecked(t *testing.T) {
	g := newGate() // no file resolver at all: a check would error
	foreign := "kb://7b4887ce51d9/.knomit/recipes/nope.js"
	out, _, err := g.Apply(context.Background(), "kb/a.md", []string{foreign}, nil)
	if err != nil {
		t.Fatalf("a foreign system file must be unchecked: %v", err)
	}
	if len(out) != 1 || out[0] != foreign {
		t.Fatalf("a foreign ref must be stored verbatim, got %v", out)
	}
}
