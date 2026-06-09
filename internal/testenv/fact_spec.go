package testenv

import (
	"fmt"

	"knomit/internal/fact"
)

// FactSpec is a fluent builder for fact content. Methods return the spec by
// value so chaining doesn't mutate shared state — the caller can branch off
// a base spec and produce variants without side effects.
//
//	base := testenv.Fact("quantum").Type(fact.Observation).Confidence(0.8)
//	a := base.Domain("physics")
//	b := base.Domain("philosophy")  // b does not inherit a's Domain
type FactSpec struct {
	title      string
	body       string
	kind       fact.Kind
	typ        fact.Type
	confidence float64
	sources    int
	domain     []string
	entities   []string
	refs       []string
}

// Fact starts a new spec with the given title. Defaults: type=Observation,
// confidence=0.5, everything else zero.
func Fact(title string) FactSpec {
	return FactSpec{
		title:      title,
		typ:        fact.Observation,
		confidence: 0.5,
	}
}

// Kind sets the fact kind (epistemic or pragmatic). Builder leaves Kind unset
// by default so SerializeFact applies the historical epistemic default — set
// this explicitly when building pragmatic facts.
func (s FactSpec) Kind(k fact.Kind) FactSpec { s.kind = k; return s }

// Type sets the leaf type. The type must be valid for the spec's Kind
// (defaulting to epistemic); SerializeFact rejects mismatched pairs.
func (s FactSpec) Type(t fact.Type) FactSpec { s.typ = t; return s }

// Confidence sets the confidence score (typically 0.0–1.0).
func (s FactSpec) Confidence(c float64) FactSpec { s.confidence = c; return s }

// Sources sets the source count.
func (s FactSpec) Sources(n int) FactSpec { s.sources = n; return s }

// Domain sets the domain list (replaces any previous domain).
func (s FactSpec) Domain(d ...string) FactSpec {
	s.domain = append([]string{}, d...)
	return s
}

// Entities sets the entity list (replaces any previous entities).
func (s FactSpec) Entities(e ...string) FactSpec {
	s.entities = append([]string{}, e...)
	return s
}

// Refs sets the refs list (replaces any previous refs).
func (s FactSpec) Refs(refs ...string) FactSpec {
	s.refs = append([]string{}, refs...)
	return s
}

// Body sets the markdown body (excluding the title heading, which is built
// from the title field).
func (s FactSpec) Body(body string) FactSpec { s.body = body; return s }

// Build serializes the spec to a fact file body (YAML frontmatter + markdown)
// using fact.SerializeFact. The path field on the underlying Fact struct is
// left as a placeholder because the writer sets the real path when committing.
//
// Panics on serialize error — Build is a test-side helper and a serialization
// failure here means the test author constructed a malformed spec, which
// should fail loudly rather than silently produce empty content.
func (s FactSpec) Build() string {
	f := fact.NewFact("placeholder.md")
	f.Title = s.title
	f.Body = s.body
	f.Kind = s.kind
	f.Type = s.typ
	f.Confidence = s.confidence
	f.Sources = s.sources
	f.Domain = s.domain
	f.Entities = s.entities
	f.Refs = s.refs
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(fmt.Sprintf("FactSpec.Build: %v", err))
	}
	return out
}
