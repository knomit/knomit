// Package synthesize implements the knomit synthesis pipeline (prune + distill).
package synthesize

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// RecipeStep is a single step in a synthesis recipe.
type RecipeStep struct {
	Mode           string  `yaml:"mode"`
	Model          string  `yaml:"model"`
	Prompt         string  `yaml:"prompt"`
	MaxDepth       int     `yaml:"max_depth"`
	Resolution     float64 `yaml:"resolution"`      // Louvain resolution (default 1.0)
	Profile        string  `yaml:"profile"`          // "large", "small", or "" (auto-detect)
	RetryOnPassive *bool   `yaml:"retry_on_passive"` // nil = use profile default
	DedupThreshold float64 `yaml:"dedup_threshold"`  // 0 = default (0.92)
}

// Scope limits which facts are gathered for synthesis.
// When nil (omitted from recipe), auto-discovery mode is used.
type Scope struct {
	Domain   []string `yaml:"domain"`
	Entities []string `yaml:"entities"`
	Search   []string `yaml:"search"`
	Path     string   `yaml:"path"`
}

// Recipe is a synthesis recipe loaded from YAML.
type Recipe struct {
	Name      string      `yaml:"name"`
	Prompt    string      `yaml:"prompt"`
	Scope     *Scope      `yaml:"scope"`
	AutoMerge bool        `yaml:"auto_merge"`
	Steps     []RecipeStep `yaml:"steps"`
}

// ParseRecipe parses a YAML recipe string into a Recipe struct.
func ParseRecipe(yml string) (Recipe, error) {
	var r Recipe
	if err := yaml.Unmarshal([]byte(yml), &r); err != nil {
		return Recipe{}, fmt.Errorf("ParseRecipe: %w", err)
	}
	if r.Name == "" {
		return Recipe{}, fmt.Errorf("ParseRecipe: name is required")
	}
	if len(r.Steps) == 0 {
		return Recipe{}, fmt.Errorf("ParseRecipe: at least one step is required")
	}
	for i, step := range r.Steps {
		if step.Mode != "prune" && step.Mode != "distill" {
			return Recipe{}, fmt.Errorf("ParseRecipe: step %d: mode must be 'prune' or 'distill', got %q", i, step.Mode)
		}
	}
	return r, nil
}
