package synthesize

import (
	"fmt"

	"knomit/internal/store"
)

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

// ScopeFilter optionally restricts the seed pool that discovery considers to
// the union of the listed domains and entities. An empty filter (zero
// domains AND zero entities) means whole-corpus, the historical default.
//
// Used by Plan 03's scope-filter feature: the connected MCP agent can ask
// "discover only inside auth/permissions" by setting Domain=[]string{"auth"}.
type ScopeFilter struct {
	Domain   []string
	Entities []string
}

// IsEmpty reports whether the filter is the zero value (whole-corpus).
func (s ScopeFilter) IsEmpty() bool {
	return len(s.Domain) == 0 && len(s.Entities) == 0
}

// Matches reports whether a fact carrying the given domains and entities falls
// within the scope. An empty filter (whole-corpus) matches everything; a
// non-empty filter matches if the fact touches at least one requested domain
// OR entity — the union semantics from the design spec (a fact is "in scope"
// if it touches any requested topic, not all of them).
//
// This is the single definition of scope membership; both the review and
// hypothesize pipelines apply it to their post-parse incremental seed pools so
// a caller-supplied scope is honored on every code path, not just first-run
// search.
func (s ScopeFilter) Matches(domains, entities []string) bool {
	if s.IsEmpty() {
		return true
	}
	for _, want := range s.Domain {
		for _, d := range domains {
			if store.DomainTagMatches(d, want) {
				return true
			}
		}
	}
	for _, want := range s.Entities {
		for _, e := range entities {
			if store.EntityTagMatches(e, want) {
				return true
			}
		}
	}
	return false
}
