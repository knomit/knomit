package mcp

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// readShipText loads a SHIP block golden file.
//
// It ERRORS on a missing file rather than skipping. The blueprint these blocks
// come from lives under .claude/, which .gitignore excludes, so these goldens
// are the only in-repo copy of the verbatim product text — and a test that
// quietly disabled itself when its subject went missing would be no test at
// all.
func readShipText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoErrorf(t, err,
		"SHIP golden %s must exist — it is the only in-repo copy of blueprint §2 text", name)
	got := strings.TrimRight(string(b), "\n")
	require.NotEmpty(t, got, "SHIP golden %s is empty; this check would pass vacuously", name)
	return got
}

// TestShipBlockA_Verbatim — the field description in both write-tool schemas
// is blueprint §2 Block A, byte for byte, and it is the SAME string in both,
// because knomit_learn and knomit_update describe one field.
func TestShipBlockA_Verbatim(t *testing.T) {
	want := readShipText(t, "motif_block_a.txt")
	require.Equal(t, want, motifFieldDescription)

	for _, tc := range []struct {
		tool   string
		schema map[string]any
	}{
		{"knomit_learn", learnToolSchemaProperties()},
		{"knomit_update", updateToolSchemaProperties()},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			prop, ok := tc.schema["motifs"].(map[string]any)
			require.True(t, ok, "%s schema must declare a motifs property", tc.tool)
			require.Equal(t, want, prop["description"])
			require.Equal(t, "array", prop["type"])
			require.Equal(t, map[string]any{"type": "string"}, prop["items"])
		})
	}
}

// TestShipBlockA_TeachesTheShapeItEnforces — the blueprint's own examples must
// survive the validator that ships alongside them. This is the check that
// would have caught the five-word zero-value-treated-as-valid before the
// designer amended it: SHIP text that teaches an example the gate rejects is a
// defect in one of the two, and the suite should say which.
func TestShipBlockA_TeachesTheShapeItEnforces(t *testing.T) {
	requireExamplesValid(t, readShipText(t, "motif_block_a.txt"))
}

// kebabToken matches any lowercase hyphenated token in SHIP prose. Every such
// token in blocks A and B today is either a motif example or an ordinary
// hyphenated word ("kebab-case", "mis-configuring"), and all of them are legal
// motif shapes — so scanning generically costs nothing and needs no list of
// examples to keep in sync with the text.
var kebabToken = regexp.MustCompile(`[a-z0-9]+(?:-[a-z0-9]+)+`)

// requireExamplesValid asserts every hyphenated token in SHIP text would
// survive the validator that ships with it. A 5+ word hyphenation trips this,
// which is exactly the case worth a human look.
func requireExamplesValid(t *testing.T, ship string) {
	t.Helper()
	found := kebabToken.FindAllString(ship, -1)
	require.NotEmpty(t, found, "no hyphenated tokens found; this check would pass vacuously")
	for _, tok := range found {
		require.NoErrorf(t, fact.ValidateMotifs([]string{tok}),
			"SHIP text teaches %q, which the shipped validator rejects — amend one or the other", tok)
	}
}
