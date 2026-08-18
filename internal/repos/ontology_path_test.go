package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// TestOntologyPathsAreCanonicalAndLegacy pins the two ontology locations and
// their single source of truth: repos.OntologyPath/LegacyOntologyPath are not
// redeclared, they are the fact package's constants under a name every
// existing caller already uses. okf/source used to carry its own duplicated
// copies of these literals — this test is what would catch that drift coming
// back.
func TestOntologyPathsAreCanonicalAndLegacy(t *testing.T) {
	require.Equal(t, ".knomit/ontology.yaml", OntologyPath)
	require.Equal(t, ".domains/ontology.yaml", LegacyOntologyPath)
	require.Equal(t, "domains/ontology.yaml", PreDotOntologyPath)
	require.True(t, strings.HasPrefix(OntologyPath, fact.PrivateRoot+"/"),
		"the canonical ontology must live in knomit's own namespace")
	// One definition, not three. okf/source used to carry its own copies.
	require.Equal(t, fact.OntologyFile, OntologyPath)
	require.Equal(t, fact.LegacyOntologyFile, LegacyOntologyPath)
	require.Equal(t, fact.PreDotOntologyFile, PreDotOntologyPath)
	// Order is newest-first and every rung is distinct: the chain is what
	// loadOntology and okf/source both walk, so a rung dropped from it is a
	// rung dropped from both readers at once.
	require.Equal(t,
		[]string{OntologyPath, LegacyOntologyPath, PreDotOntologyPath},
		fact.OntologyPathsNewestFirst())
}

// TestPrivateServerOwnedPathsAreNotAgentWritable covers the paths the
// PRIVATE-path guard is what protects: they are private (so the guard fires)
// and not agent-writable (so it refuses). The write guard is the conjunction
// `IsPrivatePath(p) && !IsWritablePrivatePath(p)`, so BOTH halves have to be
// asserted — !IsWritablePrivatePath alone is also true of README.md, and
// asserting only that would certify a protection those files do not get from
// this guard.
func TestPrivateServerOwnedPathsAreNotAgentWritable(t *testing.T) {
	for _, p := range []string{OntologyPath, LegacyOntologyPath} {
		require.Truef(t, fact.IsPrivatePath(p),
			"%s must be private, or the write guard never fires on it", p)
		require.Falsef(t, fact.IsWritablePrivatePath(p),
			"%s is server-owned and must not be writable through the fact tools", p)
	}
}

// TestNonPrivateServerOwnedPathsAreCoveredByTheirOwnGuard is the other half.
// README.md, LICENSE and the pre-dot ontology have NO dot segment, so
// fact.IsPrivatePath is false and the private-path guard never fires on them —
// they are server-owned all the same, and IsServerOwnedPath is what the fact
// endpoints check to keep them out.
//
// The distinction is the whole point of splitting this test: the old single
// assertion (!IsWritablePrivatePath on all four) passed for README.md and
// LICENSE while claiming they were unreachable through the write guard, which
// they were not.
func TestNonPrivateServerOwnedPathsAreCoveredByTheirOwnGuard(t *testing.T) {
	for _, p := range []string{ReadmePath, LicensePath, PreDotOntologyPath} {
		require.Falsef(t, fact.IsPrivatePath(p),
			"%s has no dot segment, so the private-path guard cannot be what protects it", p)
		require.Truef(t, IsServerOwnedPath(p),
			"%s is server-owned and must be refused by the fact endpoints", p)
	}
}

// IsServerOwnedPath must match the way the store actually resolves these
// names, and only those.
func TestIsServerOwnedPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// A fact path is lowercased before it reaches git, so "README.md"
		// plants "readme.md" — a SEPARATE root file that a git provider,
		// resolving the name case-insensitively, renders as the repository's
		// README. Both spellings have to be refused.
		{"README.md", true},
		{"readme.md", true},
		{"ReAdMe.Md", true},
		{"LICENSE", true},
		{"license", true},
		{OntologyPath, true},
		{LegacyOntologyPath, true},
		{PreDotOntologyPath, true},

		// Reusing a server-owned name as a DIRECTORY destroys the file just as
		// surely: git replaces the blob with a tree.
		{PreDotOntologyPath + "/x.md", true},
		{"README.md/x.md", true},

		// Ordinary facts that merely look adjacent.
		{"kb/architecture/readme-rendering/a1b2c3d4.md", false},
		{"kb/decisions/licensing/a1b2c3d4.md", false},
		{"domains/ontology.yaml.md", false},
		{"readme.md.bak", false},
		{"docs/README.md", false},
		{"", false},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, IsServerOwnedPath(c.path), "path %q", c.path)
	}
}

// TestLoadOntology_ReadsCanonicalPath seeds ONLY the canonical path with a
// distinguishable, non-preset-derived ontology (so the boot-time refresh
// cannot fire and mask a bug in the read itself) and asserts the repo loads
// exactly that ontology rather than falling through to fact.DefaultOntology.
func TestLoadOntology_ReadsCanonicalPath(t *testing.T) {
	dir, agentBranch := bootKnomitWithStaleOntologyAt(t, OntologyPath, canonicalWinsYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	require.Equal(t, "canonical-wins", ri.Ontology().ID,
		"the seeded canonical ontology must be loaded, not the default")
	require.NotEqual(t, fact.DefaultOntology().ID, ri.Ontology().ID)
}

// TestLoadOntology_FallsBackToLegacyPath seeds ONLY the legacy .domains/
// path — an unmigrated repo the user has not hand-moved yet — and asserts the
// repo still validates against ITS OWN taxonomy. Silently switching to the
// default here is exactly the failure this fallback exists to prevent: new
// facts would start validating against the wrong ontology with nothing in the
// logs tying them to the cause.
func TestLoadOntology_FallsBackToLegacyPath(t *testing.T) {
	dir, agentBranch := bootKnomitWithLegacyOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	require.Equal(t, "source-code", ri.Ontology().ID,
		"the legacy ontology must be honoured, not replaced by the default")
}

// TestLoadOntology_FallsBackToPreDotPath covers the OLDEST rung of the chain.
// .domains/ was introduced six days before .knomit/, so "a repo that has not
// been hand-migrated" overwhelmingly means one still holding
// domains/ontology.yaml — not one holding .domains/. Dropping that rung sends
// such a repo to fact.DefaultOntology() behind a single log.Warn, and every
// fact written afterwards is validated against the wrong taxonomy with nothing
// tying the bad facts to the cause. That is precisely the failure the fallback
// exists to prevent.
func TestLoadOntology_FallsBackToPreDotPath(t *testing.T) {
	dir, agentBranch := bootKnomitWithPreDotOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	require.Equal(t, "source-code", ri.Ontology().ID,
		"the pre-dot ontology must be honoured, not replaced by the default")
	require.NotEqual(t, fact.DefaultOntology().ID, ri.Ontology().ID)
}

// The write-back contract extends to the third rung: a pre-dot repo must not
// grow a second or third ontology file, with nothing distinguishing the stale
// copies from the live one. Same guarantee as
// TestLoadOntology_PresetRefreshWritesBackToThePathItRead, one rung older.
func TestLoadOntology_PresetRefreshWritesBackToThePreDotPath(t *testing.T) {
	dir, agentBranch := bootKnomitWithPreDotOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		preDot, rerr := svc.Facts().ReadFact(context.Background(), agentBranch, PreDotOntologyPath, nil)
		require.NoError(t, rerr)
		require.Contains(t, preDot.Content, "incidents",
			"the refresh must have fired and landed on the path it read from")

		for _, other := range []string{OntologyPath, LegacyOntologyPath} {
			exists, eerr := svc.Facts().FactExists(context.Background(), agentBranch, other)
			require.NoError(t, eerr)
			require.Falsef(t, exists, "a pre-dot repo must not grow an ontology file at %s", other)
		}
	}))
}

// TestLoadOntology_PresetRefreshWritesBackToThePathItRead is THE regression
// guard for this task. The repo holds ONLY a legacy-path ontology that is a
// strict subset of the embedded preset, so the boot-time refresh in
// loadOntology fires and rewrites it. If that write went to the canonical
// path instead of srcPath, a legacy repo would end up holding TWO ontology
// files, with nothing distinguishing the stale one from the live one.
func TestLoadOntology_PresetRefreshWritesBackToThePathItRead(t *testing.T) {
	dir, agentBranch := bootKnomitWithLegacyOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		// The refresh must have fired: staleCodeOntologyYAML has one topic
		// ("invariants"); the embedded preset has eight, including
		// "incidents". Seeing "incidents" on the legacy path proves the
		// refresh ran, not just that the read succeeded.
		legacy, rerr := svc.Facts().ReadFact(context.Background(), agentBranch, LegacyOntologyPath, nil)
		require.NoError(t, rerr)
		require.Contains(t, legacy.Content, "incidents",
			"the refresh must have fired and landed on the path it read from")

		exists, eerr := svc.Facts().FactExists(context.Background(), agentBranch, OntologyPath)
		require.NoError(t, eerr)
		require.False(t, exists,
			"a legacy repo must not grow a second, canonical-path ontology file")
	}))
}
