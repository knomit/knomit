package synthesize

import (
	"strings"
	"testing"
)

func TestRenderTemplate(t *testing.T) {
	data := PromptData{
		Facts: `[{"path":"test.md","title":"Test"}]`,
	}

	out, err := RenderTemplate("prune", "user", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out == "" {
		t.Fatal("empty output")
	}
	if !strings.Contains(out, data.Facts) {
		t.Error("output missing facts")
	}
}

func TestRenderTemplate_SystemPrompt(t *testing.T) {
	data := PromptData{}
	out, err := RenderTemplate("prune", "system", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out == "" {
		t.Fatal("empty system prompt")
	}
}

func TestRenderTemplate_InvalidOperation(t *testing.T) {
	_, err := RenderTemplate("nonexistent", "user", PromptData{})
	if err == nil {
		t.Error("expected error for invalid operation")
	}
}

func TestRenderTemplate_AllTemplatesExist(t *testing.T) {
	ops := []string{"prune", "distill"}
	types := []string{"system", "user", "retry"}

	for _, op := range ops {
		for _, tp := range types {
			_, err := RenderTemplate(op, tp, PromptData{Facts: "[]"})
			if err != nil {
				t.Errorf("RenderTemplate(%s, %s) failed: %v", op, tp, err)
			}
		}
	}
}
