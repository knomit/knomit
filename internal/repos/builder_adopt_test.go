package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestEnsureBranch_AdoptsAgentBranchOnRestoredHome regression-tests issue #32:
// when a knomit home is restored onto a different machine — or a repo database
// is copied — the new instance generates a fresh SSH key, so its agent branch
// name (derived from the key fingerprint) does not exist in the copied
// database. ensureBranch used to seed the branch from itself
// (CreateBranch(agent, agent)), which cannot succeed when the branch is absent,
// so the branch was never created and EVERY write path on that repo broke.
//
// The fix: when the configured agent branch is absent, adopt the repo's current
// HEAD branch (the previous machine's agent branch) as the seed source, the
// same way a fresh clone bootstraps its agent ref from origin/main.
func TestEnsureBranch_AdoptsAgentBranchOnRestoredHome(t *testing.T) {
	dir := t.TempDir()

	// --- machine 1: boot the default repo under the old key's agent branch ---
	const oldAgent = "agent/oldhost-0badf00d"
	m1 := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: oldAgent,
	})
	require.NoError(t, m1.Start())

	ri1 := m1.Get(config.DefaultRepoName)
	require.NotNil(t, ri1)

	// Accumulate local knowledge on the old agent branch so we can prove the
	// adopted branch inherits it (rather than seeding from an empty upstream).
	_, err := testService(t, ri1).Facts().WriteFact(
		context.Background(),
		oldAgent,
		"kb/notes/local-only.md",
		"a fact that only ever lived on the previous machine's agent branch",
		"test: seed local-only fact",
		"created",
	)
	require.NoError(t, err)

	// Give the old branch a custom ontology (unique id → not a preset, so the
	// refresh path leaves it untouched). ensureBranch must run before
	// loadOntology so the restored instance reads THIS ontology off the adopted
	// branch rather than falling back to the default on the first boot.
	const customOntologyYAML = `id: restored-home-custom
name: Restored Home Custom
description: an ontology that only existed on the previous machine
topics:
  invariants:
    description: Load-bearing rules
`
	_, err = testService(t, ri1).Facts().WriteFact(
		context.Background(),
		oldAgent,
		"domains/ontology.yaml",
		customOntologyYAML,
		"test: seed custom ontology",
		"updated",
	)
	require.NoError(t, err)
	require.NoError(t, m1.Close())

	// --- machine 2: restore the same home under a DIFFERENT agent branch ---
	// (fresh id_ed25519 → different fingerprint → different branch name).
	const newAgent = "agent/newhost-cafebabe"
	m2 := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: newAgent,
	})
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })

	ri2 := m2.Get(config.DefaultRepoName)
	require.NotNil(t, ri2)
	svc := testService(t, ri2)

	// ensureBranch runs before loadOntology, so the restored instance must load
	// the ontology off the adopted branch — not fall back to the default.
	require.NotNil(t, ri2.Ontology())
	require.Equal(t, "restored-home-custom", ri2.Ontology().ID,
		"restored home must load the adopted branch's ontology, not the default; issue #32")

	// The new agent branch must now exist (ensureBranch adopted it).
	head, err := svc.Branches().HeadCommit(context.Background(), newAgent)
	require.NoError(t, err, "new agent branch must exist after restore; issue #32")
	require.NotEmpty(t, head)

	// Adoption must preserve the previous machine's accumulated knowledge.
	res, err := svc.Facts().ReadFact(context.Background(), newAgent, "kb/notes/local-only.md", nil)
	require.NoError(t, err, "adopted branch must inherit the previous agent branch's facts")
	require.Contains(t, res.Content, "only ever lived on the previous machine")

	// The write path must work on the restored home — the real bug in #32.
	_, err = svc.Facts().WriteFact(
		context.Background(),
		newAgent,
		"kb/notes/after-restore.md",
		"a fact written by the new machine after the home was restored",
		"test: write after restore",
		"created",
	)
	require.NoError(t, err, "write path must work on the restored home; issue #32")
}
