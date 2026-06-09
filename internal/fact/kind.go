package fact

import "fmt"

// Kind classifies a fact as descriptive (epistemic) or prescriptive
// (pragmatic). Every Type belongs to exactly one Kind.
type Kind string

const (
	Epistemic Kind = "epistemic"
	Pragmatic Kind = "pragmatic"
)

// DefaultKind is the Kind used when a fact is parsed without a `kind`
// field. Epistemic preserves backward compatibility with every existing
// fact file authored before pragmatic facts existed.
const DefaultKind = Epistemic

// Validate reports whether k is a well-known Kind.
func (k Kind) Validate() error {
	switch k {
	case Epistemic, Pragmatic:
		return nil
	}
	return fmt.Errorf("invalid kind %q: must be one of epistemic, pragmatic", k)
}

// AllowsType reports whether t is a valid leaf Type for this Kind.
func (k Kind) AllowsType(t Type) bool {
	switch k {
	case Epistemic:
		return EpistemicTypes[t]
	case Pragmatic:
		return PragmaticTypes[t]
	}
	return false
}

// validateKindAndType normalizes a missing kind to DefaultKind, verifies the
// kind is well-known, and confirms that t is a valid leaf Type for that kind.
// Returns the normalized Kind on success.
//
// This is the single validation entry point shared by ParseFact and
// SerializeFact, so any Fact that round-trips successfully is guaranteed to
// carry a valid (kind, type) pair.
func validateKindAndType(k Kind, t Type) (Kind, error) {
	if k == "" {
		k = DefaultKind
	}
	if err := k.Validate(); err != nil {
		return "", err
	}
	if !k.AllowsType(t) {
		return "", fmt.Errorf("invalid type %q for kind %q", t, k)
	}
	return k, nil
}

// validateBounds enforces the numeric field invariants shared by ParseFact
// and SerializeFact: confidence must lie in [0, 1] and sources must be
// non-negative. Symmetric with validateKindAndType — any Fact that
// round-trips successfully is guaranteed to carry in-range values, so no
// write path (MCP, synthesize, web) can persist an out-of-range fact.
func validateBounds(confidence float64, sources int) error {
	if confidence < 0 || confidence > 1 {
		return fmt.Errorf("confidence %v out of range [0,1]", confidence)
	}
	if sources < 0 {
		return fmt.Errorf("sources %d must be >= 0", sources)
	}
	return nil
}
