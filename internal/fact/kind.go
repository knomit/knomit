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
