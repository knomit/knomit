package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateMotifs_Shape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		motif string
		ok    bool
	}{
		{"two words", "capital-influx", true},
		{"three words", "derived-state-liability", true},
		{"four words", "atomic-write-via-rename", true},
		// The Block B example, as amended by designer ruling 2026-08-21 to fit
		// the measured 2-4 contract.
		{"block b example", "zero-value-as-valid", true},
		{"one word", "collision", false},
		{"five words", "zero-value-treated-as-valid", false},
		{"uppercase", "Capital-Influx", false},
		{"space separated", "capital influx", false},
		{"snake case", "capital_influx", false},
		{"empty", "", false},
		{"leading hyphen", "-capital-influx", false},
		{"trailing hyphen", "capital-influx-", false},
		{"double hyphen", "capital--influx", false},
		{"leading space", " capital-influx", false},
		{"digits allowed", "http2-head-of-line", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMotifs([]string{tc.motif})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidateMotifs_Count(t *testing.T) {
	require.NoError(t, ValidateMotifs(nil))
	require.NoError(t, ValidateMotifs([]string{"a-b", "c-d", "e-f"}))
	err := ValidateMotifs([]string{"a-b", "c-d", "e-f", "g-h"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "maximum of 3")
}

func TestValidateMotifs_Duplicates(t *testing.T) {
	err := ValidateMotifs([]string{"capital-influx", "capital-influx"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

// TestSerializeFact_RejectsBadMotifs — the write gate. This is the MN4
// property: no per-path check anywhere, because every write path renders
// through here.
func TestSerializeFact_RejectsBadMotifs(t *testing.T) {
	f := NewFact("kb/gotchas/build/x.md")
	f.Title = "A title"
	f.Type = Observation
	f.Confidence = 0.9
	f.Sources = 1
	f.Motifs = []string{"onlyoneword"}

	_, err := SerializeFact(f)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SerializeFact")
}

// TestParseFact_DropsBadMotifsSilently — the read side is LENIENT, matching
// how HEAD already treats refs and origin: a version that was legal when it
// was committed must stay readable forever. A malformed motif renders inert
// rather than making the fact unloadable.
func TestParseFact_DropsBadMotifsSilently(t *testing.T) {
	raw := "---\n" +
		"type: observation\n" +
		"domain: [build]\n" +
		"confidence: 0.9\n" +
		"sources: 1\n" +
		"entities: []\n" +
		"motifs: [capital-influx, onlyoneword, derived-state-liability]\n" +
		"refs: []\n" +
		"---\n# A title\n\nBody.\n"

	f, err := ParseFact("kb/gotchas/build/x.md", raw)
	require.NoError(t, err)
	require.Equal(t, []string{"capital-influx", "derived-state-liability"}, f.Motifs)
}

// TestParseFact_TrimsOverCapSilently — same reasoning as above for count.
func TestParseFact_TrimsOverCapSilently(t *testing.T) {
	raw := "---\n" +
		"type: observation\n" +
		"domain: []\n" +
		"confidence: 0.9\n" +
		"sources: 1\n" +
		"entities: []\n" +
		"motifs: [a-b, c-d, e-f, g-h]\n" +
		"refs: []\n" +
		"---\n# A title\n\nBody.\n"

	f, err := ParseFact("kb/gotchas/build/x.md", raw)
	require.NoError(t, err)
	require.Equal(t, []string{"a-b", "c-d", "e-f"}, f.Motifs)
}

// TestParseFact_AllInvalidMotifsParseAsNil — nil and empty must stay
// indistinguishable (see Fact.Motifs), or a fact whose every motif was
// dropped serializes differently from one that never had any.
func TestParseFact_AllInvalidMotifsParseAsNil(t *testing.T) {
	raw := "---\n" +
		"type: observation\n" +
		"domain: []\n" +
		"confidence: 0.9\n" +
		"sources: 1\n" +
		"entities: []\n" +
		"motifs: [onlyoneword]\n" +
		"refs: []\n" +
		"---\n# A title\n\nBody.\n"

	f, err := ParseFact("kb/gotchas/build/x.md", raw)
	require.NoError(t, err)
	require.Nil(t, f.Motifs)

	out, err := SerializeFact(f)
	require.NoError(t, err, "a fact whose motifs were all dropped must still be writable")
	require.NotContains(t, out, "motifs")
}
