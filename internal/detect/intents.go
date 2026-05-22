// Package detect provides capture-worthy signal detection for transcript
// blocks. Intents and thresholds are loaded from YAML so no patterns are
// hardcoded in Go source.
package detect

import (
	_ "embed"
	"fmt"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed intents_code.yaml
var codeIntentsYAML []byte

// IntentSet groups the detection intents and their thresholds for one
// profile (e.g. "code"). Loaded from YAML at service startup.
type IntentSet struct {
	Intents    map[string]*Intent `yaml:"intents"`
	Thresholds Thresholds         `yaml:"thresholds"`
}

// Intent is a named conversation pattern to detect.
type Intent struct {
	Description      string   `yaml:"description"`
	CanonicalPhrases []string `yaml:"canonical_phrases"`
}

// Thresholds gate when a detected signal should fire a nudge.
type Thresholds struct {
	IntentScore       float64 `yaml:"intent_score"`
	NoveltyScore      float64 `yaml:"novelty_score"`
	CombinedLowIntent float64 `yaml:"combined_low_intent"`
}

// Parse parses and validates an IntentSet from YAML.
func Parse(data []byte) (*IntentSet, error) {
	var is IntentSet
	if err := yaml.Unmarshal(data, &is); err != nil {
		return nil, fmt.Errorf("parse intents: %w", err)
	}
	if len(is.Intents) == 0 {
		return nil, fmt.Errorf("parse intents: at least one intent is required")
	}
	for name, intent := range is.Intents {
		if len(intent.CanonicalPhrases) == 0 {
			return nil, fmt.Errorf("parse intents: %q has no canonical_phrases", name)
		}
	}
	return &is, nil
}

var (
	codeIntents     *IntentSet
	codeIntentsOnce sync.Once
)

// CodeIntents returns the embedded `code` intent set.
// Panics if the embedded YAML is invalid.
func CodeIntents() *IntentSet {
	codeIntentsOnce.Do(func() {
		is, err := Parse(codeIntentsYAML)
		if err != nil {
			panic(fmt.Sprintf("embedded code intents YAML is invalid: %v", err))
		}
		codeIntents = is
	})
	return codeIntents
}

// IntentsByProfile returns the IntentSet for a profile name.
// Returns an error for unknown profiles.
func IntentsByProfile(name string) (*IntentSet, error) {
	switch name {
	case "code":
		return CodeIntents(), nil
	default:
		return nil, fmt.Errorf("unknown profile: %q", name)
	}
}
