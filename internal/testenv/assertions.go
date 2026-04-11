package testenv

import (
	"math"
	"sort"
	"strings"
	"testing"
)

// Assertion helpers used by FactHandle field accessors. Each helper carries
// the captured value, a *testing.T, and a label for failure messages. All
// Must* methods return the receiver so chaining works:
//
//	f.Confidence().MustBeBetween(0.5, 1.0).MustBeGreaterThan(0.7)

// ── StringAssert ──────────────────────────────────────────────────────────

// StringAssert chains string equality / containment assertions.
type StringAssert struct {
	t     *testing.T
	got   string
	label string
}

// MustEqual asserts the string equals want exactly.
func (a *StringAssert) MustEqual(want string) *StringAssert {
	a.t.Helper()
	if a.got != want {
		a.t.Fatalf("%s: got %q, want %q", a.label, a.got, want)
	}
	return a
}

// MustContain asserts the string contains substr.
func (a *StringAssert) MustContain(substr string) *StringAssert {
	a.t.Helper()
	if !strings.Contains(a.got, substr) {
		a.t.Fatalf("%s: %q does not contain %q", a.label, a.got, substr)
	}
	return a
}

// MustHavePrefix asserts the string starts with prefix.
func (a *StringAssert) MustHavePrefix(prefix string) *StringAssert {
	a.t.Helper()
	if !strings.HasPrefix(a.got, prefix) {
		a.t.Fatalf("%s: %q does not have prefix %q", a.label, a.got, prefix)
	}
	return a
}

// Get returns the captured string value, useful when assertion helpers
// are not the right fit.
func (a *StringAssert) Get() string { return a.got }

// ── FloatAssert ───────────────────────────────────────────────────────────

// FloatAssert chains float equality / range assertions. Equality uses a
// fixed epsilon of 1e-9 — sufficient for the confidence scores knomit
// stores (REAL column, IEEE 754 round-trip through SQLite).
type FloatAssert struct {
	t     *testing.T
	got   float64
	label string
}

// MustEqual asserts the value equals want within 1e-9.
func (a *FloatAssert) MustEqual(want float64) *FloatAssert {
	a.t.Helper()
	if math.Abs(a.got-want) > 1e-9 {
		a.t.Fatalf("%s: got %v, want %v (tolerance 1e-9)", a.label, a.got, want)
	}
	return a
}

// MustBeBetween asserts the value is within [lo, hi] inclusive.
func (a *FloatAssert) MustBeBetween(lo, hi float64) *FloatAssert {
	a.t.Helper()
	if a.got < lo || a.got > hi {
		a.t.Fatalf("%s: got %v, want in [%v, %v]", a.label, a.got, lo, hi)
	}
	return a
}

// MustBeGreaterThan asserts the value is strictly greater than n.
func (a *FloatAssert) MustBeGreaterThan(n float64) *FloatAssert {
	a.t.Helper()
	if a.got <= n {
		a.t.Fatalf("%s: got %v, want > %v", a.label, a.got, n)
	}
	return a
}

// Get returns the captured float value.
func (a *FloatAssert) Get() float64 { return a.got }

// ── IntAssert ─────────────────────────────────────────────────────────────

// IntAssert chains integer equality / range assertions.
type IntAssert struct {
	t     *testing.T
	got   int
	label string
}

// MustEqual asserts the int equals want.
func (a *IntAssert) MustEqual(want int) *IntAssert {
	a.t.Helper()
	if a.got != want {
		a.t.Fatalf("%s: got %d, want %d", a.label, a.got, want)
	}
	return a
}

// Get returns the captured int value.
func (a *IntAssert) Get() int { return a.got }

// ── StringSliceAssert ─────────────────────────────────────────────────────

// StringSliceAssert chains assertions over a []string. Order-insensitive
// comparisons are the default for MustEqual — fact fields like Domain,
// Entities, Refs have no meaningful order and YAML round-trips may
// shuffle them.
type StringSliceAssert struct {
	t     *testing.T
	got   []string
	label string
}

// MustBeEmpty asserts the slice is empty (nil or zero-length).
func (a *StringSliceAssert) MustBeEmpty() *StringSliceAssert {
	a.t.Helper()
	if len(a.got) != 0 {
		a.t.Fatalf("%s: expected empty, got %v", a.label, a.got)
	}
	return a
}

// MustContain asserts every value in s appears at least once in the slice.
// Duplicates in s do not require duplicates in got.
func (a *StringSliceAssert) MustContain(s ...string) *StringSliceAssert {
	a.t.Helper()
	have := map[string]bool{}
	for _, x := range a.got {
		have[x] = true
	}
	for _, want := range s {
		if !have[want] {
			a.t.Fatalf("%s: missing %q (have: %v)", a.label, want, a.got)
		}
	}
	return a
}

// MustNotContain asserts no value in s appears in the slice.
func (a *StringSliceAssert) MustNotContain(s ...string) *StringSliceAssert {
	a.t.Helper()
	have := map[string]bool{}
	for _, x := range a.got {
		have[x] = true
	}
	for _, unwanted := range s {
		if have[unwanted] {
			a.t.Fatalf("%s: unexpectedly contains %q (have: %v)", a.label, unwanted, a.got)
		}
	}
	return a
}

// MustEqual asserts the slice contains exactly `want` (order-insensitive).
// If order matters, use Get() and compare directly.
func (a *StringSliceAssert) MustEqual(want ...string) *StringSliceAssert {
	a.t.Helper()
	g := append([]string{}, a.got...)
	w := append([]string{}, want...)
	sort.Strings(g)
	sort.Strings(w)
	if !stringSlicesEqual(g, w) {
		a.t.Fatalf("%s: got %v, want %v (order-insensitive)", a.label, a.got, want)
	}
	return a
}

// MustHaveLen asserts the slice has exactly n elements.
func (a *StringSliceAssert) MustHaveLen(n int) *StringSliceAssert {
	a.t.Helper()
	if len(a.got) != n {
		a.t.Fatalf("%s: got len %d, want %d (%v)", a.label, len(a.got), n, a.got)
	}
	return a
}

// Get returns the captured slice.
func (a *StringSliceAssert) Get() []string { return a.got }

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
