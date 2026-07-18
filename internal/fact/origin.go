package fact

import "fmt"

// Origin records how a fact came to exist. It is orthogonal to Kind
// (epistemic/pragmatic) and Type (the leaf classification):
//
//   - authored:   hand-written by a human or an agent via knomit_learn.
//   - distilled:  produced by the synthesis pipeline from existing facts
//     (today's `type: synthesis` facts).
//   - discovered: emergent — inferred by the discovery engine; a fact nobody
//     wrote down (keystones and consequences). See the emergent-fact-discovery
//     design spec.
type Origin string

const (
	Authored   Origin = "authored"
	Distilled  Origin = "distilled"
	Discovered Origin = "discovered"
)

// DefaultOrigin is used when a fact is parsed without an `origin` field and
// no type-aware override applies. Authored preserves backward compatibility
// with every fact file written before origin existed.
const DefaultOrigin = Authored

// Validate reports whether o is a well-known Origin.
func (o Origin) Validate() error {
	switch o {
	case Authored, Distilled, Discovered:
		return nil
	}
	return fmt.Errorf("invalid origin %q: must be one of authored, distilled, discovered", o)
}

// ValidateForType checks that o is a legal origin for a fact of leaf type t.
// Machine origins are reserved for the types their pipelines emit: distill
// work-items produce synthesis facts; discovery bridges produce synthesis
// (forward) or hypothesis (backward) facts. A fact an agent writes itself —
// including one transcribed from an external source it read — is authored,
// whatever its type.
func (o Origin) ValidateForType(t Type) error {
	switch o {
	case Distilled:
		if t != Synthesis {
			return fmt.Errorf("origin %q is reserved for synthesis-pipeline output (type synthesis, got %q); a fact you write yourself — including one transcribed from a source you read — is origin authored: omit the field", o, t)
		}
	case Discovered:
		if t != Synthesis && t != Hypothesis {
			return fmt.Errorf("origin %q is reserved for discovery-engine output (type synthesis or hypothesis, got %q); a fact you write yourself — including one transcribed from a source you read — is origin authored: omit the field", o, t)
		}
	}
	return nil
}
