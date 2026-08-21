package mcp

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
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

// TestShipBlockB_Verbatim — the instructions section is blueprint §2 Block B,
// byte for byte, and it actually reaches what a session is served.
func TestShipBlockB_Verbatim(t *testing.T) {
	want := readShipText(t, "motif_block_b.txt")
	require.Equal(t, want, strings.TrimRight(motifInstructionsSection, "\n"))

	require.Contains(t, ProfileInstructions("code", "kb", nil), want,
		"Block B must appear in the served instructions, not merely exist as a constant")
}

func TestShipBlockB_TeachesTheShapeItEnforces(t *testing.T) {
	requireExamplesValid(t, readShipText(t, "motif_block_b.txt"))
}

// TestMN1_InstructionsAreCorpusIndependent — the write path stays light.
// Instructions must be byte-identical whatever the corpus holds: no served
// vocabulary, no examples mined from the repo, no counts.
//
// The comparison is on BYTES rather than a grep for a motif string, because a
// grep only catches the spelling it was written to expect; a templating change
// that interpolated corpus data in some other shape would slip past it.
func TestMN1_InstructionsAreCorpusIndependent(t *testing.T) {
	empty := newInstructionsTestRepo(t, nil)
	rich := newInstructionsTestRepo(t, []motifSeed{
		{path: "kb/alpha/one.md", motifs: []string{"silent-fallback", "config-drift"}},
		{path: "kb/alpha/two.md", motifs: []string{"silent-fallback"}},
		{path: "kb/beta/three.md", motifs: []string{"unmonitored-expiry"}},
	})

	for _, profile := range []string{"code", "chat", "generic"} {
		t.Run(profile, func(t *testing.T) {
			a := ProfileInstructions(profile, empty.OntologyRoot(), empty.Ontology())
			b := ProfileInstructions(profile, rich.OntologyRoot(), rich.Ontology())
			require.Equal(t, a, b,
				"MN1: server instructions must not vary with corpus content")
			require.Contains(t, a, "### Motifs",
				"the comparison is worthless if neither side carries the section")
		})
	}
}

// TestMN1_NoVocabularyInAnyPrompt — nothing in the served surface may
// enumerate this corpus's motifs. Phase 2 introduces exactly one exception
// (the backfill work item); until then the count is zero.
func TestMN1_NoVocabularyInAnyPrompt(t *testing.T) {
	const marker = "zzz-unique-marker"
	rich := newInstructionsTestRepo(t, []motifSeed{
		{path: "kb/alpha/one.md", motifs: []string{marker}},
	})
	require.NotContains(t,
		ProfileInstructions("code", rich.OntologyRoot(), rich.Ontology()), marker)

	for name, schema := range map[string]map[string]any{
		"knomit_learn":  learnToolSchemaProperties(),
		"knomit_update": updateToolSchemaProperties(),
	} {
		for prop, spec := range schema {
			m, ok := spec.(map[string]any)
			if !ok {
				continue
			}
			desc, _ := m["description"].(string)
			require.NotContainsf(t, desc, marker, "%s.%s leaks corpus vocabulary", name, prop)
		}
	}
}

// TestMN1_FrontmatterListNamesMotifs — the pointer bullet must be present.
// Its absence is not cosmetic: the frontmatter list is an enumeration, and an
// agent that reads it as complete will treat an unlisted motifs: as cruft on
// a fact it is updating — which, since knomit_update replaces list fields
// wholesale, deletes them.
func TestMN1_FrontmatterListNamesMotifs(t *testing.T) {
	instr := ProfileInstructions("code", "kb", nil)
	require.Contains(t, instr, "- **motifs**:")
	require.Less(t, strings.Index(instr, "- **motifs**:"), strings.Index(instr, "### Motifs"),
		"the pointer bullet must come before the section it points at")
}

// motifSeed is one fact to plant in a corpus fixture.
type motifSeed struct {
	path   string
	motifs []string
}

// newInstructionsTestRepo opens a fresh store, writes the seeded facts, and
// returns the instance. The seeds are REAL committed facts, not a stub: MN1 is
// a claim about what happens when a corpus actually holds motifs, and a
// fixture that never wrote any would make the comparison trivially true.
func newInstructionsTestRepo(t *testing.T, seeds []motifSeed) *repos.RepoInstance {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	for _, seed := range seeds {
		f := fact.NewFact(seed.path)
		f.Title = "T " + seed.path
		f.Body = "Body of " + seed.path
		f.Type = fact.Observation
		f.Domain = []string{"alpha"}
		f.Entities = []string{"Widget"}
		f.Refs = []string{}
		f.Confidence = 0.8
		f.Sources = 1
		f.Motifs = seed.motifs
		body, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), "agent/test", f.Path(), body, "seed", "")
		require.NoError(t, err)
	}

	// The seeds must have LANDED, or every assertion below is about an empty
	// corpus wearing a rich corpus's name.
	if len(seeds) > 0 {
		n, err := svc.Search().TokenDF(context.Background(), "agent/test", seeds[0].motifs[0], "motif")
		require.NoError(t, err)
		require.Positive(t, n, "seeded motifs must be indexed, or this fixture proves nothing")
	}

	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		UID:          nextTestRepoUID(),
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})
}
