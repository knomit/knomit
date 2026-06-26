package synthesize

import "fmt"

// Effort dials how hard a pipeline digs for emergent facts nobody wrote down.
// normal reproduces today's behaviour exactly; medium and high engage the
// structural-bridge discovery engine (more candidate seed sets at high).
//
// Vocabulary is load-bearing API surface across MCP, the session DB, and the
// synthesize package; do not rename mid-flight.
type Effort string

const (
	EffortNormal Effort = "normal"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
)

// DefaultEffort is what an empty/absent argument resolves to. Normal preserves
// today's byte-for-byte behaviour, satisfying the regression invariant in
// emergent-fact-discovery's plan.
const DefaultEffort = EffortNormal

// Validate reports whether e is a well-known Effort.
func (e Effort) Validate() error {
	switch e {
	case EffortNormal, EffortMedium, EffortHigh:
		return nil
	}
	return fmt.Errorf("invalid effort %q: must be one of normal, medium, high", e)
}

// Discovers reports whether this effort level engages the discovery engine.
// EffortNormal never does; medium and high do.
func (e Effort) Discovers() bool {
	return e == EffortMedium || e == EffortHigh
}

// NormalizeEffort returns e if it is well-known, or DefaultEffort if e is the
// empty string. Callers at trust boundaries should still Validate() — this
// helper exists for downstream code that has already validated upstream and
// just needs to coerce "" → normal without re-checking the enum.
func NormalizeEffort(e Effort) Effort {
	if e == "" {
		return DefaultEffort
	}
	return e
}
