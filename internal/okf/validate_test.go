// internal/okf/validate_test.go
package okf

import (
	"testing"
	"time"
)

func TestValidate_GoodBundlePasses(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	f := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf]
---
# X

Body.`, ts)
	b, _ := Build(RepoIdentity{ID: "x"}, []FactInput{f}, nil)
	if err := Validate(b); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidate_RejectsConceptWithoutType(t *testing.T) {
	b := Bundle{Files: []File{
		{Path: "index.md", Content: []byte("---\nokf_version: \"0.1\"\n---\n\n# Root\n")},
		{Path: "log.md", Content: []byte("# Log\n\nNo changes recorded.\n")},
		{Path: "a/bad.md", Content: []byte("---\ntitle: no type here\n---\n\n# Bad\n")},
	}}
	if err := Validate(b); err == nil {
		t.Fatal("expected validation error for missing type")
	}
}

func TestValidate_RejectsUnparseableFrontmatter(t *testing.T) {
	b := Bundle{Files: []File{
		{Path: "index.md", Content: []byte("---\nokf_version: \"0.1\"\n---\n")},
		{Path: "log.md", Content: []byte("# Log\n")},
		{Path: "a/broken.md", Content: []byte("no frontmatter at all\n")},
	}}
	if err := Validate(b); err == nil {
		t.Fatal("expected validation error for missing frontmatter")
	}
}
