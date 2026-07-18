package testenv

import "math/rand"

// OpGen is a deterministic random-op generator used by the property
// tests in test/storytests/property_test.go. Given the same seed,
// every method produces the same sequence of values across runs, which
// is what makes a failing property test reproducible: on failure the
// test logs the seed, and a rerun with KNOMIT_PROPTEST_SEED=<seed>
// replays the exact same operations.
//
// OpGen is intentionally tiny — only the primitives the property tests
// need are exposed. Tests compose them into higher-level op sequences.
type OpGen struct {
	r *rand.Rand
}

// NewOpGen returns a deterministic OpGen seeded at `seed`. Tests
// typically derive the seed from KNOMIT_PROPTEST_SEED with a time-based
// fallback so each CI run gets fresh coverage while failures remain
// reproducible.
func NewOpGen(seed int64) *OpGen {
	return &OpGen{r: rand.New(rand.NewSource(seed))}
}

// Intn returns a deterministic pseudo-random int in [0, n). Panics if
// n <= 0 — callers that pass a computed n must guard against that.
func (g *OpGen) Intn(n int) int { return g.r.Intn(n) }

// Float64 returns a deterministic pseudo-random float64 in [0.0, 1.0).
func (g *OpGen) Float64() float64 { return g.r.Float64() }

// Choose picks one element from a slice deterministically. Panics on
// an empty slice — callers must always pass at least one option.
func (g *OpGen) Choose(options []string) string {
	if len(options) == 0 {
		panic("OpGen.Choose: empty options slice")
	}
	return options[g.r.Intn(len(options))]
}

// Bool returns a deterministic pseudo-random bool.
func (g *OpGen) Bool() bool { return g.r.Intn(2) == 1 }
