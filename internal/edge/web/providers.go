package web

// storeProviders bundles every injectable data-access seam the API router
// wires into handlers. Before this existed the Server carried separate
// unexported fields and NewAPIRouter carried near-identical
// nil-check-then-default blocks interleaved with route registrations — the
// wiring was scattered through 300 lines of routing and two of the locals
// collided on name (sp/sp2).
//
// The zero value means "all defaults": callers materialize the production
// implementations with withDefaults(). Tests set only the members they stub.
//
// This is a construction-time wiring artifact, scoped to Server and
// NewAPIRouter — deliberately NOT a runtime service locator. Handlers keep
// their narrow constructor signatures so each one's true dependency set stays
// visible at its registration site; passing the bundle around would make
// every handler nominally depend on every seam.
type storeProviders struct {
	factWriter FactWriter
	origin     originProvider
}

// withDefaults returns a copy with every nil member replaced by its
// production implementation.
//
// The value receiver is load-bearing: the Server's stored bundle is never
// mutated, so a test's sparse overrides stay sparse and NewAPIRouter can be
// called twice on the same Server with identical results.
func (p storeProviders) withDefaults() storeProviders {
	if p.factWriter == nil {
		p.factWriter = defaultFactWriter{}
	}
	if p.origin == nil {
		p.origin = defaultOriginProvider{}
	}
	return p
}
