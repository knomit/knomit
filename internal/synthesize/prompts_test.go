package synthesize

import (
	"strings"
	"testing"
)

func TestRenderTemplate(t *testing.T) {
	data := PromptData{
		Facts:        `[{"file":"test.md","title":"Test"}]`,
		RecipePrompt: "summarize everything",
		StepPrompt:   "be thorough",
	}

	out, err := RenderTemplate("large", "prune", "user", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out == "" {
		t.Fatal("empty output")
	}
	if !strings.Contains(out, data.Facts) {
		t.Error("output missing facts")
	}
	if !strings.Contains(out, data.RecipePrompt) {
		t.Error("output missing recipe prompt")
	}
}

func TestRenderTemplate_SmallHasExample(t *testing.T) {
	data := PromptData{Facts: `[]`}
	out, err := RenderTemplate("small", "prune", "user", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if !strings.Contains(out, "EXAMPLE OUTPUT") {
		t.Error("small prune_user should contain EXAMPLE OUTPUT")
	}
}

func TestRenderTemplate_SystemPrompt(t *testing.T) {
	data := PromptData{}
	out, err := RenderTemplate("large", "prune", "system", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out == "" {
		t.Fatal("empty system prompt")
	}
}

func TestRenderTemplate_InvalidProfile(t *testing.T) {
	_, err := RenderTemplate("nonexistent", "prune", "user", PromptData{})
	if err == nil {
		t.Error("expected error for invalid profile")
	}
}

func TestRenderTemplate_AllTemplatesExist(t *testing.T) {
	profiles := []string{"large", "small"}
	ops := []string{"prune", "distill"}
	types := []string{"system", "user", "retry"}

	for _, p := range profiles {
		for _, op := range ops {
			for _, tp := range types {
				_, err := RenderTemplate(p, op, tp, PromptData{Facts: "[]"})
				if err != nil {
					t.Errorf("RenderTemplate(%s, %s, %s) failed: %v", p, op, tp, err)
				}
			}
		}
	}
}
