package synthesize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// §2.1's LLM half: every fact-producing path carries motifs, and every one of
// them flows through SerializeFact so the count, shape and subject strip apply
// with no per-path code (MN4).

// All three derived-writer schemas must advertise motifs with the carry-over
// instruction. A schema that omits it silently drops the axis at exactly the
// moment a fact is being replaced by another.
func TestDerivedWriters_AllThreeSchemasCarryMotifs(t *testing.T) {
	for name, schema := range map[string]string{
		"prune":    pruneResponseSchema,
		"distill":  distillResponseSchema,
		"discover": discoverResponseSchema,
	} {
		require.Containsf(t, schema, `"motifs"`, "%s schema must accept motifs", name)
		require.Containsf(t, schema, "general regularity",
			"%s schema must tell the model what a motif IS — a field with no "+
				"explanation gets filled with subject words", name)
		require.Containsf(t, schema, "zero is correct",
			"%s schema must say zero is correct, or a model treats the field as "+
				"mandatory and invents one", name)
	}
}

// Prune and distill say CARRY OVER; discover says the seed's motif is a
// candidate. The difference is deliberate — a merged fact replaces its members
// and should inherit what stays true, while a discovered fact is a new claim
// whose regularity may not be the seed's at all.
func TestDerivedWriters_CarryOverWordingDiffersForDiscovery(t *testing.T) {
	require.Contains(t, pruneResponseSchema, "Carry over member motifs")
	require.Contains(t, distillResponseSchema, "Carry over member motifs")
	require.NotContains(t, discoverResponseSchema, "Carry over member motifs",
		"a discovered fact is a NEW claim, not a replacement — telling it to carry "+
			"member motifs over would stamp the seed's regularity onto a consequence "+
			"that may not instantiate it")
	require.Contains(t, discoverResponseSchema, "natural candidate")
}

// factForLLM must SHOW motifs, or the judges cannot carry over what they never
// saw.
func TestDerivedWriters_FactForLLMShowsMotifs(t *testing.T) {
	f := factForLLM{File: "kb/a.md", Motifs: []string{"silent-fallback"}}
	blob, err := json.Marshal([]factForLLM{f})
	require.NoError(t, err)
	require.Contains(t, string(blob), "silent-fallback")

	// ...and omits the key entirely when there are none, so a motif-less corpus
	// sends the same payload it always did.
	blob, err = json.Marshal([]factForLLM{{File: "kb/b.md"}})
	require.NoError(t, err)
	require.NotContains(t, string(blob), "motifs")
}

// MN11 / §2.1: no mechanical stamping of the seed's motif onto a discovered
// fact. The prompt may suggest it; nothing in the code may apply it.
func TestDerivedWriters_NoMechanicalSeedStamping(t *testing.T) {
	src := readSourceFile(t, "discovery.go")
	// The proposal's own motifs are read; the SEED's are never copied onto it.
	require.Contains(t, src, "f.Motifs = p.Motifs",
		"a discovered fact carries the motifs the model proposed")
	for _, forbidden := range []string{"seed.Motifs", "Seed.Motifs", "seedMotifs"} {
		require.NotContainsf(t, src, forbidden,
			"discovery must not stamp the seed's motif mechanically (%s): wrong for a "+
				"consequence whose regularity is not the seed's, and futile for a "+
				"keystone, whose subject IS the mechanism", forbidden)
	}
}

// MN4: no derived writer validates motifs itself. The count, the shape and the
// subject strip live in SerializeFact and nowhere else — which is what the
// fact-package conformance test enforces globally; this is the local statement
// of the same rule for the three paths added here.
func TestDerivedWriters_NoLocalMotifValidation(t *testing.T) {
	for _, rel := range []string{"decision.go", "discovery.go"} {
		src := readSourceFile(t, rel)
		for _, gate := range []string{"ValidateMotifs", "DropInvalidMotifs", "StripSubjectMotifs", "MaxMotifs"} {
			require.NotContainsf(t, src, gate,
				"%s applies the motif gate itself; route through SerializeFact instead "+
					"(MN4 — validation has ONE entry point)", rel)
		}
		require.NotContains(t, strings.ToLower(src), "len(motifs) >",
			"a hand-rolled count check is per-path validation by another name")
	}
}

// §2.1: keystones stay motif-less BY DESIGN, via the ordinary subject strip and
// with no exemption.
//
// A backward keystone's subject IS the mechanism, so a motif naming that
// mechanism restates the fact's own subject and the strip removes it — exactly
// as the field contract says it should. The point of the test is that this
// needs no special case: the same rule that stops an ordinary fact from
// tagging itself with its own subject also keeps keystones clean, and adding
// an exemption would let a keystone carry a motif that says nothing.
func TestDerivedWriters_KeystoneMotifIsStrippedByTheOrdinaryRule(t *testing.T) {
	f := fact.NewFact("kb/meta/reasoning/silent-fallback.md")
	f.Title = "Silent fallback"
	f.Body = "The mechanism itself, as a keystone."
	f.Type = fact.Synthesis
	f.Domain = []string{"silent-fallback"}
	f.Entities = []string{"silent fallback"}
	f.Refs = []string{}
	f.Confidence = 0.8
	f.Sources = 1
	// A motif naming the keystone's own subject.
	f.Motifs = []string{"silent-fallback", "config-drift"}

	rendered, err := fact.SerializeFact(f)
	require.NoError(t, err)
	require.NotContains(t, rendered, "silent-fallback\n",
		"a motif restating the keystone's subject must be stripped")

	parsed, err := fact.ParseFact("kb/meta/reasoning/silent-fallback.md", rendered)
	require.NoError(t, err)
	require.Equal(t, []string{"config-drift"}, parsed.Motifs,
		"the subject motif is dropped SILENTLY and the unrelated one survives — no "+
			"exemption needed, and none wanted")
}

// A merged fact's motifs go through SerializeFact like every other write, so
// an over-cap or subject-restating list from the judge is handled by the one
// gate rather than by prune.
func TestDerivedWriters_MergedFactMotifsFlowThroughSerializeFact(t *testing.T) {
	f := fact.NewFact("kb/alpha/merged.md")
	f.Title = "Merged claim"
	f.Body = "Body."
	f.Type = fact.Observation
	f.Domain = []string{"alpha"}
	f.Entities = []string{"Widget"}
	f.Refs = []string{}
	f.Confidence = 0.8
	f.Sources = 2
	// Four motifs: over the cap of 3.
	f.Motifs = []string{"silent-fallback", "config-drift", "cache-stampede", "clock-skew"}

	_, err := fact.SerializeFact(f)
	require.Error(t, err,
		"an over-cap list must be refused at the single entry point — a writer is an "+
			"agent that can retry, and a silent trim would lose an authored motif")
}
