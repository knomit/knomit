package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// craftedOntology is a symlink target whose TEXT is itself a parseable
// ontology. A reader that read the link text as content would find a usable
// ontology in it, so only a reader that refuses the symlink passes.
const craftedOntology = "id: crafted\nname: Crafted\ntopics:\n  notes:\n    description: N\n"

func symlinkAt(t *testing.T, svc *Service, branch, path, target string) plumbing.Hash {
	t.Helper()
	h, err := svc.RawSymlinkForTest(context.Background(), branch, path, target, "symlink "+path)
	require.NoError(t, err)
	return plumbing.NewHash(h)
}

func executableAt(t *testing.T, svc *Service, branch, path, content string) plumbing.Hash {
	t.Helper()
	h, err := svc.RawExecutableForTest(context.Background(), branch, path, content, "+x "+path)
	require.NoError(t, err)
	return plumbing.NewHash(h)
}

// requireSymlinkOntologyError: the error wraps fact.ErrSymlinkNotFollowed,
// names the path, says symlinks are not followed, and is never "no ontology".
func requireSymlinkOntologyError(t *testing.T, err error, path string) {
	t.Helper()
	require.ErrorIs(t, err, fact.ErrSymlinkNotFollowed)
	require.NotErrorIs(t, err, ErrNoOntologyAtCommit)
	require.Contains(t, err.Error(), path+" is a symlink")
	require.Contains(t, err.Error(), "does not follow symlinks")
}

// A symlinked ontology is an ERROR naming its path at every store reader of
// the ontology (user ruling 2026-10-08, "d"): OntologyAtCommit (triggers,
// guidance), OntologyAt and OntologyFileAt (repo open, create-time check,
// consensus, sync and conflicts settings) and treeOntology (fleet, verify).
// A legacy rung holding a regular ontology does not cover for it: the first
// rung present is the ontology, and it is a symlink.
// Sabotage: OntologyAtCommit or treeOntologyFile back on IsFile → the crafted
// link text is returned as the ontology → red.
func TestOntologyReaders_SymlinkIsANamedError(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()

	regular := putRaw(t, svc, "main", fact.OntologyFile, "id: real\n")
	putRaw(t, svc, "main", fact.LegacyOntologyFile, "id: legacy\n")
	p, _, data, err := tr.OntologyAtCommit(ctx, regular)
	require.NoError(t, err)
	require.Equal(t, fact.OntologyFile, p)
	require.Equal(t, "id: real\n", string(data), "a regular ontology is read")

	tip := symlinkAt(t, svc, "main", fact.OntologyFile, craftedOntology)

	_, _, data, err = tr.OntologyAtCommit(ctx, tip)
	requireSymlinkOntologyError(t, err, fact.OntologyFile)
	require.Nil(t, data)

	data, err = svc.OntologyAt(ctx, "main")
	requireSymlinkOntologyError(t, err, fact.OntologyFile)
	require.Nil(t, data)

	p, data, err = svc.OntologyFileAt(ctx, "main")
	requireSymlinkOntologyError(t, err, fact.OntologyFile)
	require.Empty(t, p)
	require.Nil(t, data)

	_, err = svc.FleetMembersAt("main")
	requireSymlinkOntologyError(t, err, fact.OntologyFile)
}

// A symlinked LEGACY rung, with no canonical ontology, is the same error
// naming that rung.
func TestOntologyReaders_SymlinkedLegacyRungIsANamedError(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tip := symlinkAt(t, svc, "main", fact.LegacyOntologyFile, craftedOntology)

	_, _, _, err := svc.Triggers().OntologyAtCommit(ctx, tip)
	requireSymlinkOntologyError(t, err, fact.LegacyOntologyFile)
	_, err = svc.OntologyAt(ctx, "main")
	requireSymlinkOntologyError(t, err, fact.LegacyOntologyFile)
}

// An executable (+x) ontology is a file, read like a regular one, at both
// store readers. Sabotage: isSystemFileMode refusing Executable → red.
func TestOntologyReaders_ExecutableIsAFile(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tip := executableAt(t, svc, "main", fact.OntologyFile, "id: x\n")

	p, _, data, err := svc.Triggers().OntologyAtCommit(ctx, tip)
	require.NoError(t, err)
	require.Equal(t, fact.OntologyFile, p)
	require.Equal(t, "id: x\n", string(data))

	p, data, err = svc.OntologyFileAt(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, fact.OntologyFile, p)
	require.Equal(t, "id: x\n", string(data))
}

// A symlinked trigger script or recipe is ABSENT (ErrNoScriptAtCommit,
// ErrNoRecipeAtCommit: no new error class), even when the file it names sits
// right beside it; that file is still read; an executable one is a file.
// Sabotage: privateFileAt back on IsFile → the link text comes back as the
// JS source → red.
func TestPrivateFileAt_SymlinkIsAbsentExecutableIsAFile(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()
	putRaw(t, svc, "main", fact.TriggerScriptPath("real"), "SCRIPT")
	putRaw(t, svc, "main", fact.TriggerRecipePath("real"), "RECIPE")
	symlinkAt(t, svc, "main", fact.TriggerScriptPath("link"), "real.js")
	symlinkAt(t, svc, "main", fact.TriggerRecipePath("link"), "real.js")
	executableAt(t, svc, "main", fact.TriggerScriptPath("exec"), "EXEC SCRIPT")
	tip := executableAt(t, svc, "main", fact.TriggerRecipePath("exec"), "EXEC RECIPE")

	_, data, err := tr.ScriptAt(ctx, tip, "link")
	require.ErrorIs(t, err, ErrNoScriptAtCommit, "a symlinked script is absent")
	require.Nil(t, data)
	_, data, err = tr.RecipeAt(ctx, tip, "link")
	require.ErrorIs(t, err, ErrNoRecipeAtCommit, "a symlinked recipe is absent")
	require.Nil(t, data)

	for _, c := range []struct {
		read func(context.Context, plumbing.Hash, string) (string, []byte, error)
		name string
		want string
	}{
		{tr.ScriptAt, "real", "SCRIPT"},
		{tr.RecipeAt, "real", "RECIPE"},
		{tr.ScriptAt, "exec", "EXEC SCRIPT"},
		{tr.RecipeAt, "exec", "EXEC RECIPE"},
	} {
		blob, data, err := c.read(ctx, tip, c.name)
		require.NoError(t, err, c.name)
		require.Equal(t, c.want, string(data))
		require.Len(t, blob, 40)
	}
}

// An executable (+x) system file is served by SystemFileAt, and an executable
// SKILL.md is a skill (the gap NB1 of the #438 review: refusing Executable
// stayed green). Sabotage: isSystemFileMode refusing Executable → red.
func TestSystemFileAndSkill_ExecutableIsAFile(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	executableAt(t, svc, "main", ".knomit/run.sh", "#!/bin/sh\n")
	tip := executableAt(t, svc, "main", ".knomit/skills/ex/SKILL.md", "EXEC SKILL")

	sf, found, err := svc.SystemFiles().SystemFileAt(ctx, "main", tip.String(), ".knomit/run.sh", 1<<10)
	require.NoError(t, err)
	require.True(t, found, "an executable system file is a file")
	require.Equal(t, "#!/bin/sh\n", string(sf.Content))

	skills, err := svc.Skills().SkillsAt(ctx, tip)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	require.Equal(t, "ex", skills[0].Dir)
	require.Equal(t, "EXEC SKILL", string(skills[0].Data))
}
