// Package fact provides shared domain types for the knomit knowledge base.
package fact

// Type is the leaf value of a fact's classification. A Type belongs to
// exactly one Kind (epistemic or pragmatic), enforced by Kind.AllowsType.
type Type string

// Epistemic leaf types — descriptive knowledge ("what is").
const (
	Observation Type = "observation"
	Concept     Type = "concept"
	Process     Type = "process"
	Principle   Type = "principle"
	Pattern     Type = "pattern"
	Reference   Type = "reference"
	Synthesis   Type = "synthesis"
	Insight     Type = "insight"
	Hypothesis  Type = "hypothesis"
	Methodology Type = "methodology"
)

// EpistemicTypes is the authoritative set of epistemic Types.
var EpistemicTypes = map[Type]bool{
	Observation: true,
	Concept:     true,
	Process:     true,
	Principle:   true,
	Pattern:     true,
	Reference:   true,
	Synthesis:   true,
	Insight:     true,
	Hypothesis:  true,
	Methodology: true,
}

// AllEpistemicTypes returns all epistemic Types in a stable order.
func AllEpistemicTypes() []Type {
	return []Type{Observation, Concept, Process, Principle, Pattern, Reference, Synthesis, Insight, Hypothesis, Methodology}
}

// DefaultEpistemicType is the leaf type used when an epistemic fact is
// parsed without a `type` field. It preserves the historical default.
const DefaultEpistemicType = Observation
