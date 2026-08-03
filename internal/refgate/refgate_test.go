package refgate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const gateRepoID = "3ec012f5b4d2"

// existsIn builds a ResolveFunc over a fixed set, case-folded the way storage
// is. Standing in for FactExistsAt, whose real answer includes facts reachable
// only by walking back past a retraction.
func existsIn(paths ...string) ResolveFunc {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[strings.ToLower(p)] = true
	}
	return func(_ context.Context, path string) (bool, error) {
		return set[strings.ToLower(path)], nil
	}
}

func newGate(paths ...string) Gate { return New(gateRepoID, existsIn(paths...)) }

func TestGate_AcceptsExistingTarget(t *testing.T) {
	g := newGate("kb/decisions/x/abc.md")
	batch := map[string][]string{
		"kb/gotchas/y/new.md": {"kb/decisions/x/abc.md"},
	}
	if err := g.CheckBatch(context.Background(), batch, nil); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

// The case that makes the gate ergonomic rather than a burden: three facts
// written in ONE call, all citing each other, none existing yet. All must be
// accepted, because BatchWriteFacts commits them together.
func TestGate_AcceptsIntraBatchCrossReferences(t *testing.T) {
	g := newGate()
	batch := map[string][]string{
		"kb/a.md": {"kb/b.md", "kb/c.md"},
		"kb/b.md": {"kb/c.md"},
		"kb/c.md": {"kb/a.md"}, // circular — still fine, one commit
	}
	if err := g.CheckBatch(context.Background(), batch, nil); err != nil {
		t.Fatalf("intra-batch cross-references must be accepted, got %v", err)
	}
}

func TestGate_RejectsMissingTarget(t *testing.T) {
	g := newGate()
	batch := map[string][]string{
		"kb/gotchas/y/new.md": {"kb/nope.md"},
	}
	err := g.CheckBatch(context.Background(), batch, nil)
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
func TestGate_ReportsAllProblems(t *testing.T) {
	g := newGate()
	batch := map[string][]string{
		"kb/a.md": {"kb/nope1.md", "kb/nope2.md"},
		"kb/b.md": {"kb/nope3.md"},
	}
	err := g.CheckBatch(context.Background(), batch, nil)
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
func TestGate_AcceptsQualifiedSelfReference(t *testing.T) {
	g := newGate("kb/decisions/x/abc.md")
	batch := map[string][]string{
		"kb/y/new.md": {"kb://3ec012f5b4d2/kb/decisions/x/abc.md"},
	}
	if err := g.CheckBatch(context.Background(), batch, nil); err != nil {
		t.Fatalf("qualified self-reference must be accepted, got %v", err)
	}
}

// Foreign, source, and external refs are never gated — knomit cannot check them.
func TestGate_IgnoresUncheckableKinds(t *testing.T) {
	g := newGate()
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
	if err := g.CheckBatch(context.Background(), batch, nil); err != nil {
		t.Fatalf("uncheckable ref kinds must pass, got %v", err)
	}
}

// SUBSUMPTION. A fact that retracts what it supersedes cites the retracted path
// as lineage, in the same commit — knomit_learn's observation-subsumes-
// hypothesis path and distill's synthesis-subsumes-sources path both do exactly
// this. It needs no special case: the target is still live at the pre-write
// head, so it simply resolves.
func TestGate_AcceptsRefToFactRetractedByTheSameCall(t *testing.T) {
	g := newGate("kb/old.md") // live at the pre-write head; the delete lands with this commit
	batch := map[string][]string{"kb/new.md": {"kb/old.md"}}
	if err := g.CheckBatch(context.Background(), batch, nil); err != nil {
		t.Fatalf("subsumption lineage must be accepted, got %v", err)
	}
}

// TEMPORAL CONTRACT, part 1: refs a fact ALREADY carried are never re-checked.
//
// kb/principles/philosophy/historical-not-current — a ref resolves at the
// commit of its referrer, never at HEAD. Re-judging a carried-forward ref means
// one retraction anywhere in history makes every fact that ever cited it
// uneditable: changing a title would be refused over a citation written months
// earlier, which is the gate rewriting the past.
func TestGate_DoesNotRecheckCarriedForwardRefs(t *testing.T) {
	g := New(gateRepoID, func(context.Context, string) (bool, error) {
		return false, nil // nothing resolves any more
	})
	batch := map[string][]string{"kb/citing.md": {"kb/long-gone.md"}}
	prior := map[string][]string{"kb/citing.md": {"kb/long-gone.md"}}

	if err := g.CheckBatch(context.Background(), batch, prior); err != nil {
		t.Fatalf("a ref the fact already carried must never be re-judged, got %v", err)
	}

	// The exemption is per-ref, not per-fact: a NEW ref in the same write is
	// still checked, or "add one good ref" would launder an unresolvable one.
	batch["kb/citing.md"] = []string{"kb/long-gone.md", "kb/brand-new-typo.md"}
	if err := g.CheckBatch(context.Background(), batch, prior); err == nil {
		t.Fatal("a newly added unresolvable ref must still be rejected")
	}
}

// A carried-forward ref is recognised across FORMS: the fact stored it bare and
// the write resends it canonical (or vice versa). Comparing raw strings would
// call it new and re-judge it.
func TestGate_CarriedForwardExemptionIsFormAgnostic(t *testing.T) {
	g := New(gateRepoID, func(context.Context, string) (bool, error) {
		return false, nil
	})
	batch := map[string][]string{"kb/citing.md": {"kb://3ec012f5b4d2/kb/long-gone.md"}}
	prior := map[string][]string{"kb/citing.md": {"kb/long-gone.md"}}
	if err := g.CheckBatch(context.Background(), batch, prior); err != nil {
		t.Fatalf("the same ref in canonical form is still carried forward, got %v", err)
	}
}

// TEMPORAL CONTRACT, part 2: the gate asks the READER's question.
//
// A retracted fact still has a navigable last-valid blob, so FactExistsAt
// resolves it and the UI renders a ref to it as a live `fact` link. The gate
// must agree — a narrower "is it live right now" check would reject writes the
// rest of the system resolves perfectly well.
func TestGate_AcceptsRefResolvableOnlyByWalkBack(t *testing.T) {
	// The resolver stands in for FactExistsAt: this path has no live row, but
	// walking back from HEAD finds its last valid version.
	g := New(gateRepoID, func(_ context.Context, path string) (bool, error) {
		return path == "kb/retracted-last-week.md", nil
	})
	batch := map[string][]string{"kb/new.md": {"kb/retracted-last-week.md"}}
	if err := g.CheckBatch(context.Background(), batch, nil); err != nil {
		t.Fatalf("a ref the reader resolves must not be rejected by the gate, got %v", err)
	}
}

// learn passes the authoritative on-disk path, which preserves the configured
// ontology root verbatim — "KB/..." for an uppercase root — while ClassifyRef
// lowercases. The batch membership check must fold case or an intra-batch
// cross-reference under an uppercase root would be wrongly rejected.
func TestGate_BatchMembershipIsCaseFolded(t *testing.T) {
	g := newGate()
	batch := map[string][]string{
		"KB/decisions/x/One.md": {"KB/decisions/x/Two.md"},
		"KB/decisions/x/Two.md": {},
	}
	if err := g.CheckBatch(context.Background(), batch, nil); err != nil {
		t.Fatalf("uppercase ontology root must not break batch membership: %v", err)
	}
}

// A gate with no resolver must refuse rather than wave a local ref through —
// a misconfigured caller should fail loudly, not silently.
func TestGate_NoResolveFuncIsAnError(t *testing.T) {
	g := New(gateRepoID, nil)
	err := g.CheckBatch(context.Background(), map[string][]string{"kb/a.md": {"kb/b.md"}}, nil)
	if err == nil {
		t.Fatal("want an error when no resolver is configured")
	}
}

// An author never needs to know a repo id: bare paths are qualified on write.
func TestCanonicalize(t *testing.T) {
	g := newGate()
	got, changed := g.Canonicalize([]string{
		"kb/decisions/x/abc.md",              // bare → qualified
		"kb://3ec012f5b4d2/kb/y.md",          // already canonical → unchanged
		"kb/Decisions/X/Abc.md",              // canonicalized AND lowercased
		"kb://7b4887ce51d9/kb/z.md",          // another repo → untouched
		"src://knomit/internal/x.go@ca1c272", // source → untouched
		"https://example.com/a",              // external → untouched
		"file:///tmp/b",                      // external → untouched
	})
	if !changed {
		t.Error("changed must be true when refs were rewritten")
	}

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

// changed=false is what lets a caller skip re-serializing content it would
// otherwise rewrite byte-for-byte.
func TestCanonicalize_UnchangedReportsFalse(t *testing.T) {
	g := newGate()
	_, changed := g.Canonicalize([]string{
		"kb://3ec012f5b4d2/kb/y.md",
		"https://example.com/a",
	})
	if changed {
		t.Error("already-canonical refs must report changed=false")
	}
}

// With no resolvable identity, leave bare paths alone rather than qualifying
// them with an empty or wrong id.
func TestCanonicalize_NoIDIsANoOp(t *testing.T) {
	g := New("", existsIn())
	in := []string{"kb/x.md", "https://e.com"}
	got, changed := g.Canonicalize(in)
	if changed {
		t.Error("changed must be false with no repo id")
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("[%d] = %q, want %q unchanged", i, got[i], in[i])
		}
	}
}

// Canonicalizing must not MANUFACTURE a duplicate: a fact citing the same
// target once bare and once qualified holds two spellings of one edge, which
// collapse to a single ref.
func TestCanonicalize_DedupesFormVariants(t *testing.T) {
	g := newGate()
	got, changed := g.Canonicalize([]string{
		"kb/x.md",
		"kb://3ec012f5b4d2/kb/x.md",
		"kb/X.md",
		"https://e.com/a",
		"https://e.com/a",
	})
	if !changed {
		t.Error("collapsing duplicates is a change")
	}

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

// Apply is the single-fact path the three non-batch writers share: it must
// reject exactly what CheckBatch rejects, and canonicalize exactly what
// Canonicalize canonicalizes.
func TestApply_ChecksThenCanonicalizes(t *testing.T) {
	g := newGate("kb/target.md")

	out, changed, err := g.Apply(context.Background(), "kb/new.md", []string{"kb/target.md"}, nil)
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if !changed || len(out) != 1 || out[0] != "kb://3ec012f5b4d2/kb/target.md" {
		t.Fatalf("Apply = %v (changed=%v), want the qualified form", out, changed)
	}

	if _, _, err := g.Apply(context.Background(), "kb/new.md", []string{"kb/gone.md"}, nil); err == nil {
		t.Fatal("Apply must reject an unresolvable local ref")
	}
}

// A fact may cite ITSELF (dedup-merge appends its own path as lineage) — the
// single-fact batch satisfies that without a resolution lookup.
func TestApply_SelfReferenceIsSatisfiedByTheBatch(t *testing.T) {
	g := New(gateRepoID, func(context.Context, string) (bool, error) {
		return false, errors.New("resolver must not be consulted")
	})
	if _, _, err := g.Apply(context.Background(), "kb/self.md", []string{"kb/self.md"}, nil); err != nil {
		t.Fatalf("a self-citation must be satisfied by its own batch entry, got %v", err)
	}
}
