package synthesize

import (
	"testing"
)

const testRecipeYAML = `
name: my-recipe
prompt: "summarize the knowledge base"
auto_merge: true
steps:
  - mode: prune
    model: claude-sonnet-4-6
  - mode: distill
    max_depth: 2
    resolution: 1.5
`

func TestParseRecipe(t *testing.T) {
	r, err := ParseRecipe(testRecipeYAML)
	if err != nil {
		t.Fatalf("ParseRecipe returned error: %v", err)
	}

	if r.Name != "my-recipe" {
		t.Errorf("Name: got %q, want %q", r.Name, "my-recipe")
	}
	if r.Prompt != "summarize the knowledge base" {
		t.Errorf("Prompt: got %q, want %q", r.Prompt, "summarize the knowledge base")
	}
	if !r.AutoMerge {
		t.Error("AutoMerge: got false, want true")
	}
	if r.Scope != nil {
		t.Errorf("Scope: got %+v, want nil (auto-discovery)", r.Scope)
	}
	if len(r.Steps) != 2 {
		t.Fatalf("Steps: got %d, want 2", len(r.Steps))
	}

	step0 := r.Steps[0]
	if step0.Mode != "prune" {
		t.Errorf("Steps[0].Mode: got %q, want %q", step0.Mode, "prune")
	}
	if step0.Model != "claude-sonnet-4-6" {
		t.Errorf("Steps[0].Model: got %q, want %q", step0.Model, "claude-sonnet-4-6")
	}

	step1 := r.Steps[1]
	if step1.Mode != "distill" {
		t.Errorf("Steps[1].Mode: got %q, want %q", step1.Mode, "distill")
	}
	if step1.MaxDepth != 2 {
		t.Errorf("Steps[1].MaxDepth: got %d, want 2", step1.MaxDepth)
	}
	if step1.Resolution != 1.5 {
		t.Errorf("Steps[1].Resolution: got %f, want 1.5", step1.Resolution)
	}
}

func TestParseRecipeWithProfile(t *testing.T) {
	yml := `
name: profiled
steps:
  - mode: prune
    profile: small
    retry_on_passive: false
  - mode: distill
`
	r, err := ParseRecipe(yml)
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	if r.Steps[0].Profile != "small" {
		t.Errorf("Steps[0].Profile: got %q, want %q", r.Steps[0].Profile, "small")
	}
	if r.Steps[0].RetryOnPassive == nil || *r.Steps[0].RetryOnPassive != false {
		t.Errorf("Steps[0].RetryOnPassive: want pointer to false")
	}
	if r.Steps[1].Profile != "" {
		t.Errorf("Steps[1].Profile: got %q, want empty", r.Steps[1].Profile)
	}
	if r.Steps[1].RetryOnPassive != nil {
		t.Errorf("Steps[1].RetryOnPassive: want nil")
	}
}

func TestParseRecipeValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name:    "missing name",
			yaml:    "steps:\n  - mode: prune\n",
			wantErr: true,
		},
		{
			name:    "no steps",
			yaml:    "name: foo\n",
			wantErr: true,
		},
		{
			name:    "invalid mode",
			yaml:    "name: foo\nsteps:\n  - mode: invalid\n",
			wantErr: true,
		},
		{
			name: "valid with scope",
			yaml: `name: scoped
steps:
  - mode: prune
scope:
  domain: [testing]
  path: know/test/
`,
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRecipe(tc.yaml)
			if (err != nil) != tc.wantErr {
				t.Errorf("ParseRecipe error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
