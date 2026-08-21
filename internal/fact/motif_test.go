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

func stripFixture() Fact {
	f := NewFact("kb/gotchas/integrations/antigravity/plugin-dir-resolution/e5d04257.md")
	f.Title = "A title"
	f.Body = "Body."
	f.Type = Observation
	f.Domain = []string{"integrations", "build tooling"}
	f.Entities = []string{"Antigravity", "mcp_config.json"}
	f.Refs = []string{}
	f.Confidence = 0.9
	f.Sources = 1
	return f
}

func TestStripSubjectMotifs_DropsEntitySubsets(t *testing.T) {
	f := stripFixture()
	// "antigravity-shadowing" -> {antigravity, shadowing}: NOT a subset
	// (shadowing is not a subject token), so it survives.
	// "antigravity-plugin-resolution" -> {antigravity, plugin, resolution}:
	// every token is an entity or path token, so it is the fact's own subject
	// wearing a motif's clothes.
	f.Motifs = []string{"antigravity-shadowing", "antigravity-plugin-resolution"}
	require.Equal(t, []string{"antigravity-shadowing"}, StripSubjectMotifs(f))
}

func TestStripSubjectMotifs_DropsDomainSubsets(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"build-tooling"}
	require.Nil(t, StripSubjectMotifs(f))
}

func TestStripSubjectMotifs_DropsPathSubsets(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"plugin-dir-resolution"}
	require.Nil(t, StripSubjectMotifs(f))
}

func TestStripSubjectMotifs_IsStemmed(t *testing.T) {
	f := stripFixture()
	f.Entities = []string{"vulnerabilities"}
	f.Domain = []string{"scanning"}
	f.Motifs = []string{"vulnerability-scanning"}
	require.Nil(t, StripSubjectMotifs(f), "stemming must collapse vulnerabilities/vulnerability")
}

func TestStripSubjectMotifs_KeepsGeneralRegularities(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"zero-value-as-valid", "silent-fallback", "name-collision"}
	require.Equal(t, f.Motifs, StripSubjectMotifs(f))
}

// TestStripSubjectMotifs_ExtensionIsNotASubjectToken — "md" must not enter
// the subject set, or a two-word motif ending in it would be judged against a
// token that describes the file format, not the fact.
func TestStripSubjectMotifs_ExtensionIsNotASubjectToken(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"md-rendering"}
	require.Equal(t, []string{"md-rendering"}, StripSubjectMotifs(f))
}

// TestSerializeFact_StripIsSilent — the strip NEVER errors. A caller that
// writes a subject-restating motif gets a stored fact, minus the motif.
func TestSerializeFact_StripIsSilent(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"antigravity-plugin-resolution"}

	out, err := SerializeFact(f)
	require.NoError(t, err, "the subject strip drops, it never errors")
	require.NotContains(t, out, "motifs")

	back, err := ParseFact(f.Path(), out)
	require.NoError(t, err)
	require.Nil(t, back.Motifs)
}

// TestSerializeFact_StripDoesNotMutateInput — SerializeFact is a pure
// renderer. The strip must not reach back into the caller's Fact.
func TestSerializeFact_StripDoesNotMutateInput(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"antigravity-plugin-resolution", "silent-fallback"}

	_, err := SerializeFact(f)
	require.NoError(t, err)
	require.Equal(t,
		[]string{"antigravity-plugin-resolution", "silent-fallback"}, f.Motifs)
}

// TestSerializeFact_ValidateBeforeStrip — a malformed motif must be REPORTED
// even when the strip would have removed it anyway. Ordering the strip first
// would silently swallow the caller's misunderstanding of the field.
func TestSerializeFact_ValidateBeforeStrip(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"antigravity"} // one word AND a subject word
	_, err := SerializeFact(f)
	require.Error(t, err)
	require.Contains(t, err.Error(), "kebab-case words")
}

// TestFactToJS_MotifsAreResolved — ValidateFact runs BEFORE SerializeFact, so
// a rule reading the raw field would judge motifs that are about to vanish.
// `fact.motifs.length >= 1` must not pass on a fact that will be written with
// none.
func TestFactToJS_MotifsAreResolved(t *testing.T) {
	f := stripFixture()
	f.Motifs = []string{"antigravity-plugin-resolution", "silent-fallback"}

	js := factToJS(f)
	require.Equal(t, []string{"silent-fallback"}, js["motifs"],
		"the rule sandbox must see what lands on disk, not the raw field")
}
