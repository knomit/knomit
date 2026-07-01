package testenv

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestFactHandle_ExistsAndBasicFields asserts that a written fact can be
// read back via snapshot.Fact(path), the state is Exists, and the field
// assertions return the values we wrote.
func TestFactHandle_ExistsAndBasicFields(t *testing.T) {
	t.Log("Scenario: Write fact with title/type/confidence/refs, read it back via snap.Fact, assert every field")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.Write("kb/x.md",
		Fact("alpha").
			Type(fact.Observation).
			Confidence(0.7).
			Sources(3).
			Domain("concepts", "physics").
			Entities("Alice").
			Refs("kb/b.md").
			Body("the body"),
		"add x")

	fh := snap.Fact("kb/x.md")
	require.Equal(t, FactStateExists, fh.State())
	require.Equal(t, "kb/x.md", fh.Path())
	require.Equal(t, snap.Commit, fh.Commit())

	fh.MustExist()
	fh.Title().MustEqual("alpha")
	fh.Type().MustEqual(string(fact.Observation))
	fh.Confidence().MustEqual(0.7)
	fh.Sources().MustEqual(3)
	fh.Domain().MustContain("concepts", "physics")
	fh.Entities().MustContain("Alice")
	fh.Refs().MustContain("kb/b.md")
	fh.Body().MustContain("the body")
}

// TestFactHandle_MissingAtSnapshot asserts that reading a fact path that
// doesn't exist at a snapshot produces a Missing handle that passes
// MustNotExist.
func TestFactHandle_MissingAtSnapshot(t *testing.T) {
	t.Log("Scenario: Write one fact, attempt to read a different path — handle is Missing")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.Write("kb/x.md", Fact("x"), "add x")
	fh := snap.Fact("kb/missing.md")
	require.Equal(t, FactStateMissing, fh.State())
	fh.MustNotExist()
}

// TestFactHandle_TemporalReadAtOlderCommit is a mini preview of the
// Category C temporal invariant — the full matrix lands in Task 3.4.
// Here we just assert that reading the same path at two different
// snapshot commits returns the two different historical states.
func TestFactHandle_TemporalReadAtOlderCommit(t *testing.T) {
	t.Log("Scenario: Write confidence=0.3, update to confidence=0.9; at snap1 still sees 0.3, at snap2 sees 0.9")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	c1 := agent.Write("kb/x.md", Fact("x").Confidence(0.3), "v1")
	c2 := agent.Update("kb/x.md", Fact("x").Confidence(0.9), "v2")

	c1.Fact("kb/x.md").Confidence().MustEqual(0.3)
	c2.Fact("kb/x.md").Confidence().MustEqual(0.9)
}

// TestFactHandle_OrderInsensitiveDomain is a regression guard for the
// StringSliceAssert MustContain semantics.
func TestFactHandle_OrderInsensitiveDomain(t *testing.T) {
	t.Log("Scenario: fact with multi-value domain assertions work regardless of order")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")
	snap := agent.Write("kb/x.md", Fact("x").Domain("b", "a", "c"), "add x")

	fh := snap.Fact("kb/x.md")
	fh.Domain().MustContain("a").MustContain("b", "c").MustHaveLen(3)
}
