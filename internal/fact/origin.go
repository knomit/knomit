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
