// internal/okf/golden_test.go
package okf

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "update golden files")

func fixtureFacts(t *testing.T) []FactInput {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	return []FactInput{
		factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf]
entities: [export, lens]
---
# Export scope is repo only

OKF export is per-repo and export-only in v1.`, ts),
		factInput(t, "kb/invariants/okf/refs-never-pushed/3209d651.md", `---
kind: pragmatic
type: policy
domain: [okf]
refs: ["internal/store/remote_sync.go:238"]
---
# Refs never pushed

Generated okf/* refs must never reach any remote.`, ts),
	}
}

func TestGoldenBundle(t *testing.T) {
	b, skips := Build(RepoIdentity{ID: "0123456789ab"}, fixtureFacts(t), []LogEntry{
		{Date: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC), Kind: "Creation", Title: "Export scope is repo only", Path: "kb/decisions/okf/scope/d9d6557d.md"},
	}, RenderOpts{})
	if skips.Skipped != 0 {
		t.Fatalf("unexpected skips: %+v", skips)
	}
	checkGolden(t, filepath.Join("testdata", "golden"), b)
}

// fixtureFactsRich exercises the features the minimal fixture cannot reach:
// # History (a fact with revisions), # Cited by (an internal ref), retirements
// of both kinds, the synthesis/hypothesis digests, entity hubs, a sources:
// block, status: draft, and bracket escaping in a title.
func fixtureFactsRich(t *testing.T) ([]FactInput, []LogEntry, []Retirement) {
	base := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	target := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: synthesis
domain: [okf, store]
entities: [EnsureOKF, okfWriteTree, Slug]
origin: distilled
confidence: 0.9
---
# Export scope is repo only

Body.`, base)
	target.Revisions = []Revision{
		{Date: base, Operation: "learn", Confidence: 0.5, Title: "Export scope is repo only", BodyDigest: "d1"},
		{Date: base.AddDate(0, 1, 0), Operation: "update", Confidence: 0.9, Title: "Export scope is repo only", BodyDigest: "d2"},
	}
	citer := factInput(t, "kb/invariants/store/edges/eb438c74.md", `---
kind: pragmatic
type: policy
domain: [store]
entities: [EnsureOKF, okfWriteTree, Slug]
confidence: 0.3
refs: ["kb/decisions/okf/scope/d9d6557d.md", "https://example.com/x"]
---
# refs → [:DERIVED_FROM] edges are driven from rec.Refs

Body.`, base.AddDate(0, 1, 5))
	hypo := factInput(t, "kb/decisions/okf/pred/aa11bb22.md", `---
kind: epistemic
type: hypothesis
domain: [okf]
entities: [EnsureOKF, okfWriteTree, Slug]
---
# A falsifiable prediction

Body.`, base.AddDate(0, 2, 0))

	log := []LogEntry{
		{Date: base, Kind: "Creation", Title: "Export scope is repo only", Path: "kb/decisions/okf/scope/d9d6557d.md"},
		// Carries a Delta, so the golden covers a rendered Update row. One
		// without a Delta is dropped by RenderLog — see the entry below it,
		// which pins that the drop reaches the golden too.
		{Date: base.AddDate(0, 1, 0), Kind: "Update", Title: "Export scope is repo only", Path: "kb/decisions/okf/scope/d9d6557d.md", Delta: "confidence 0.79 → 0.9"},
		{Date: base.AddDate(0, 1, 2), Kind: "Update", Title: "A retag nobody needs to read about", Path: "kb/decisions/okf/scope/d9d6557d.md"},
		{Date: base.AddDate(0, 1, 5), Kind: "Creation", Title: "refs → [:DERIVED_FROM] edges are driven from rec.Refs", Path: "kb/invariants/store/edges/eb438c74.md"},
		{Date: base.AddDate(0, 2, 0), Kind: "Creation", Title: "A falsifiable prediction", Path: "kb/decisions/okf/pred/aa11bb22.md"},
	}
	retired := []Retirement{
		{Date: base.AddDate(0, 2, 3), Title: "An older claim", Path: "kb/decisions/okf/old/99887766.md",
			Kind: RetiredSuperseded, SuccessorPath: "kb/decisions/okf/scope/d9d6557d.md"},
		{Date: base.AddDate(0, 1, 20), Title: "A withdrawn claim", Path: "kb/decisions/okf/gone/55443322.md",
			Kind: RetiredRetracted},
	}
	return []FactInput{target, citer, hypo}, log, retired
}

func TestGoldenBundleRich(t *testing.T) {
	facts, log, retired := fixtureFactsRich(t)
	b, skips := Build(RepoIdentity{ID: "0123456789ab"}, facts, log, RenderOpts{
		Retired: retired,
		Ontology: OntologyDoc{
			Name:        "Source Code Knowledge",
			Description: "Knowledge categories for AI agents working in a codebase.",
			Nodes: map[string]string{
				"decisions":        "Design choices with rationale",
				"decisions/okf":    "The OKF export surface",
				"invariants":       "Load-bearing rules that must never be violated",
				"invariants/store": "Rules the git-backed store enforces",
			},
		},
	})
	if skips.Skipped != 0 {
		t.Fatalf("unexpected skips: %+v", skips)
	}
	checkGolden(t, filepath.Join("testdata", "golden-rich"), b)
}

// checkGolden compares a bundle against a golden tree, or rewrites it under
// -update. It also asserts no STALE goldens remain: -update only writes files,
// so a renamed or removed bundle path would leave the old file behind and the
// comparison alone would never notice.
func checkGolden(t *testing.T, dir string, b Bundle) {
	t.Helper()
	if err := Validate(b); err != nil {
		t.Fatalf("golden bundle not conformant: %v", err)
	}
	for _, f := range b.Files {
		p := filepath.Join(dir, f.Path)
		if *update {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, f.Content, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing golden %s (run -update): %v", p, err)
		}
		if string(want) != string(f.Content) {
			t.Errorf("golden mismatch for %s (run -update to refresh)", f.Path)
		}
	}
	if *update {
		return
	}

	expected := map[string]bool{}
	for _, f := range b.Files {
		expected[filepath.FromSlash(f.Path)] = true
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		if !expected[rel] {
			t.Errorf("stale golden %s is no longer produced (delete testdata/golden and re-run -update)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking goldens: %v", err)
	}
}

// Determinism: two Builds of the same input are byte-identical.
func TestBuild_Deterministic(t *testing.T) {
	facts := fixtureFacts(t)
	b1, _ := Build(RepoIdentity{ID: "0123456789ab"}, facts, nil, RenderOpts{})
	b2, _ := Build(RepoIdentity{ID: "0123456789ab"}, facts, nil, RenderOpts{})
	if len(b1.Files) != len(b2.Files) {
		t.Fatalf("file count differs: %d vs %d", len(b1.Files), len(b2.Files))
	}
	for i := range b1.Files {
		if b1.Files[i].Path != b2.Files[i].Path || string(b1.Files[i].Content) != string(b2.Files[i].Content) {
			t.Fatalf("nondeterministic at %s", b1.Files[i].Path)
		}
	}
}
