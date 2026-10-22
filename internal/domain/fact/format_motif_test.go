package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMotifs_RoundTrip: a fact carrying motifs serializes them and parses
// them back identically, in authored order.
func TestMotifs_RoundTrip(t *testing.T) {
	f := NewFact("kb/gotchas/build/x.md")
	f.Title = "A title"
	f.Body = "A body."
	f.Type = Observation
	f.Domain = []string{"build"}
	f.Entities = []string{"Bazel"}
	f.Refs = []string{}
	f.Confidence = 0.9
	f.Sources = 1
	f.Motifs = []string{"silent-fallback", "config-drift"}

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.Contains(t, out, "motifs: [silent-fallback, config-drift]")

	back, err := ParseFact("kb/gotchas/build/x.md", out)
	require.NoError(t, err)
	require.Equal(t, []string{"silent-fallback", "config-drift"}, back.Motifs)
}

// TestMotifs_AbsentIsByteIdentical: the entire pre-motif corpus must
// serialize to exactly the bytes it does today. A fact with no motifs emits
// no motifs key at all — not `motifs: []`.
func TestMotifs_AbsentIsByteIdentical(t *testing.T) {
	f := NewFact("kb/gotchas/build/x.md")
	f.Title = "A title"
	f.Body = "A body."
	f.Type = Observation
	f.Domain = []string{"build"}
	f.Entities = []string{"Bazel"}
	f.Refs = []string{}
	f.Confidence = 0.9
	f.Sources = 1

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.NotContains(t, out, "motifs")

	back, err := ParseFact("kb/gotchas/build/x.md", out)
	require.NoError(t, err)
	require.Nil(t, back.Motifs, "absent motifs must parse back as nil, not []string{}")
}

// TestMotifs_EmptySliceElides: an explicitly empty list is the same as
// absent. "Omit entirely ... an empty list is a correct answer" (Block A) —
// both spellings must produce the same bytes, or two writers of the same
// fact disagree on its blob hash.
func TestMotifs_EmptySliceElides(t *testing.T) {
	f := NewFact("kb/gotchas/build/x.md")
	f.Title = "A title"
	f.Body = "A body."
	f.Type = Observation
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}
	f.Confidence = 0.9
	f.Sources = 1
	f.Motifs = []string{}

	withEmpty, err := SerializeFact(f)
	require.NoError(t, err)

	f.Motifs = nil
	withNil, err := SerializeFact(f)
	require.NoError(t, err)

	require.Equal(t, withNil, withEmpty)
}

// TestMotifs_JSONRoundTrip: Fact's custom (Un)MarshalJSON must carry motifs,
// or every JSON boundary in the repo silently drops them.
func TestMotifs_JSONRoundTrip(t *testing.T) {
	f := NewFact("kb/gotchas/build/x.md")
	f.Title = "A title"
	f.Type = Observation
	f.Motifs = []string{"silent-fallback"}

	data, err := f.MarshalJSON()
	require.NoError(t, err)
	require.Contains(t, string(data), `"motifs":["silent-fallback"]`)

	var back Fact
	require.NoError(t, back.UnmarshalJSON(data))
	require.Equal(t, []string{"silent-fallback"}, back.Motifs)
}
