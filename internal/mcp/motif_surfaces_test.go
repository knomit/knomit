package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// §6's read surfaces. The knob's DEFAULT and its GATING are the parts with
// teeth: loose tiers are for a reader who judges what comes back, and anything
// that triggers automation stays on the §5 operating points.

// The schema's enum is read from the store's tier list, so the parameter
// surface and the implementation cannot drift — the fact-schema single-source
// pattern (invariant 2061717e) applied to this knob.
func TestMotifMatch_SchemaEnumMatchesTheImplementation(t *testing.T) {
	enum := motifMatchEnum()
	require.Len(t, enum, len(store.AllMotifMatchTiers))
	for _, tier := range store.AllMotifMatchTiers {
		require.Contains(t, enum, string(tier))
	}
	require.Equal(t, "exact", enum[0], "the strictest tier is listed first and is the default")
	require.Equal(t, "soft", enum[len(enum)-1], "loosest last")
}

func TestMotifMatch_DefaultsToExact(t *testing.T) {
	tier, err := parseMotifMatch("")
	require.NoError(t, err)
	require.Equal(t, store.MotifMatchExact, tier,
		"an unspecified tier must be the STRICTEST one — a caller who did not choose "+
			"must not silently receive the noisiest results")
}

// An unrecognised tier is an ERROR, not a fall back. Falling back would answer
// a differently-scoped question while looking like it succeeded — and a typo in
// a loose tier would return the strict tier's results, which a caller checking
// for breadth cannot distinguish from "there is nothing looser".
func TestMotifMatch_UnknownTierIsRejectedNotDowngraded(t *testing.T) {
	_, err := parseMotifMatch("fuzzy")
	require.Error(t, err)
	require.Contains(t, err.Error(), "motif_match must be one of")

	_, err = parseMotifMatch("EXACT")
	require.Error(t, err, "tier names are exact strings, not case-insensitive guesses")
}

func TestMotifMatch_EveryTierParses(t *testing.T) {
	for _, tier := range store.AllMotifMatchTiers {
		got, err := parseMotifMatch(string(tier))
		require.NoErrorf(t, err, "tier %q must parse", tier)
		require.Equal(t, tier, got)
	}
}

// A motif-only query is a legitimate query: "what else instantiates this
// mechanism?" is the question the axis exists to answer.
func TestMotifQuery_MotifAloneCountsAsAFilter(t *testing.T) {
	require.True(t, hasAnyFilter(store.SearchOptions{Motifs: []string{"silent-fallback"}}))
	require.False(t, hasAnyFilter(store.SearchOptions{}),
		"...but an empty query is still an empty query")
}

// TestMotifMatch_LooseTiersAreNeverReachedByAutomation is the §6 grep: no
// non-test code may name token-1 or soft. They exist for a caller who types
// them, and for nobody else.
//
// The check is over CALL SITES, not over the constants' definitions — the
// tiers must be nameable, or the parameter could not accept them. What must
// not exist is code that SELECTS one.
func TestMotifMatch_LooseTiersAreNeverReachedByAutomation(t *testing.T) {
	for _, rel := range []string{"query.go", "explain.go", "learn.go", "update.go", "review.go"} {
		src := readMCPSource(t, rel)
		for _, loose := range []string{"MotifMatchToken1", "MotifMatchSoft", `"token-1"`, `"soft"`} {
			require.NotContainsf(t, src, loose,
				"%s names the loose tier %s. Loose tiers are reachable ONLY by explicit "+
					"caller parameter — measured at 15 false pairs on the eval set, which is "+
					"acceptable for a human reading results and never for automation.",
				rel, loose)
		}
	}
}

func readMCPSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := readFileForTest(rel)
	require.NoError(t, err)
	return string(b)
}

// The explain surface's sibling list is bounded. explain answers about ONE
// fact, and an unbounded list on a popular motif would bury it.
func TestExplainMotifs_SiblingListIsBounded(t *testing.T) {
	require.Greater(t, explainMotifsShown, 0)
	require.LessOrEqual(t, explainMotifsShown, 10,
		"the sibling budget must stay small enough that the fact being explained "+
			"remains the subject of the response")
}

// Motifs are omitted from the wire when absent, so a motif-free corpus's
// responses are byte-identical to what they were before the axis existed.
func TestMotifSurfaces_OmittedWhenAbsent(t *testing.T) {
	fm := frontmatterOutput{Domain: []string{"alpha"}}
	blob, err := marshalForTest(fm)
	require.NoError(t, err)
	require.NotContains(t, string(blob), "motifs")

	fm.Motifs = []string{"silent-fallback"}
	blob, err = marshalForTest(fm)
	require.NoError(t, err)
	require.Contains(t, string(blob), "silent-fallback")

	e := explainFactEntry{Path: "kb/a.md"}
	blob, err = marshalForTest(e)
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(string(blob)), "motifs")
}

func readFileForTest(rel string) ([]byte, error) { return os.ReadFile(filepath.Clean(rel)) }

func marshalForTest(v any) ([]byte, error) { return json.Marshal(v) }
