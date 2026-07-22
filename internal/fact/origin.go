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

// AllOrigins returns all Origins in a stable order. It is the authoritative
// enumeration for callers that must render or validate the complete set —
// notably the MCP layer, which derives the `origin` JSON-schema enum from it
// so a new Origin cannot be added here without surfacing at the protocol
// boundary. Kept in lockstep with Validate below.
func AllOrigins() []Origin {
	return []Origin{Authored, Distilled, Discovered}
}

// DefaultOrigin is used when a fact is parsed without an `origin` field and
// no type-aware override applies. Authored preserves backward compatibility
// with every fact file written before origin existed.
const DefaultOrigin = Authored

// defaultOriginForType is the single source of truth for "what does a missing
// `origin:` line mean for a fact of leaf type t". Both halves of the on-disk
// round-trip consult it: ParseFact resolves an absent field through it, and
// SerializeFact elides the field only when writing it back would be redundant
// under it.
//
// Keeping this in one function is the whole point. When the elision rule and
// the parse default were two independent expressions of the same idea they
// drifted: serialize elided every authored fact as "just the default", while
// parse resolved a missing origin on a synthesis fact to distilled. An
// authored synthesis fact therefore round-tripped to distilled, permanently
// reattributing a human's fact to the distill pipeline — visible in the
// index, the include_origins filter, and the web origin badge.
//
// Synthesis defaults to distilled because every synthesis fact written before
// the origin field existed came out of the distill pipeline; reading those
// legacy files as authored would misattribute the corpus in the other
// direction. Every other type defaults to authored.
func defaultOriginForType(t Type) Origin {
	if t == Synthesis {
		return Distilled
	}
	return DefaultOrigin
}

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
