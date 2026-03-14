// Package fact provides shared domain types for the knomit knowledge base.
package fact

import "fmt"

// EpistemicType classifies the kind of knowledge a fact represents.
type EpistemicType string

const (
	Observation EpistemicType = "observation"
	Concept     EpistemicType = "concept"
	Process     EpistemicType = "process"
	Principle   EpistemicType = "principle"
	Pattern     EpistemicType = "pattern"
	Reference   EpistemicType = "reference"
)

// validTypes is the authoritative set of allowed epistemic types.
var validTypes = map[EpistemicType]bool{
	Observation: true,
	Concept:     true,
	Process:     true,
	Principle:   true,
	Pattern:     true,
	Reference:   true,
}

// Valid reports whether t is one of the allowed epistemic types.
func (t EpistemicType) Valid() bool {
	return validTypes[t]
}

// Validate returns an error if t is not a valid epistemic type.
func (t EpistemicType) Validate() error {
	if t.Valid() {
		return nil
	}
	return fmt.Errorf("invalid epistemic type %q: must be one of observation, concept, process, principle, pattern, reference", t)
}

// AllTypes returns all valid epistemic types in a stable order.
func AllTypes() []EpistemicType {
	return []EpistemicType{Observation, Concept, Process, Principle, Pattern, Reference}
}

// DefaultType is the epistemic type used when none is specified.
const DefaultType = Observation
