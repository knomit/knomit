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
	if err := Validate(b); err != nil {
		t.Fatalf("golden bundle not conformant: %v", err)
	}

	dir := filepath.Join("testdata", "golden")
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

	// Also assert no STALE goldens remain. -update only writes files, so a
	// renamed or removed bundle path leaves the old file behind and the
	// comparison above would never notice — which is exactly how a stale copy
	// of the pre-views/ layout survived a regeneration.
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
