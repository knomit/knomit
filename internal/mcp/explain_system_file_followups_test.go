package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// #428 follow-ups. N2: a fact's ref to a .knomit/ file that is not in the
// tree at the fact's own commit is SHOWN in the walk (deleted, no blob), never
// silently dropped. N4 (user ruling 2026-10-08: "we do NOT want to follow
// symlinks, so 404"): a symlink under .knomit/ is never followed and reads as
// absent at every MCP door — direct explain, the walk, the learn gate.

const sysSymlink = ".knomit/skills/link.md" // -> program-knomit/SKILL.md, a regular file

func seedSysSymlink(t *testing.T, ri *repos.RepoInstance) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.RawSymlinkForTest(context.Background(), "agent/test", sysSymlink, "program-knomit/SKILL.md", "a symlink pushed through git")
		require.NoError(t, err)
	}))
}

// TestExplain_FileAbsentAtPinIsDeleted: the fact cites a file that is not at
// the fact's commit (written past the gate, as an either-side resolution can
// land one). The walk shows it as a system_file summary pinned at the fact's
// commit, deleted, with no blob — and still deleted after the file appears at
// the tip, because the pin, not the tip, is what the fact cited. A readable
// sibling ref in the same fact is NOT deleted.
// Sabotage: restore `continue` on !okFile in explainResume → the node is
// missing → red; mark every file child deleted → the sibling → red.
func TestExplain_FileAbsentAtPinIsDeleted(t *testing.T) {
	ri, ctx := sysFileRepo(t)
	const later = ".knomit/runs/later.txt"
	factCommit := writeExplainFact(t, ctx, ri, "kb/reference/later.md", "cites a file not yet there", 0.9,
		[]string{later, sysOntology})

	check := func(phase string) {
		nodes := explainWalk(t, ctx, "kb/reference/later.md")
		gone := asFileNode(t, nodes[later])
		require.Equalf(t, "system_file", gone.Kind, "%s", phase)
		require.Truef(t, gone.Summary, "%s", phase)
		require.Equalf(t, 1, gone.Depth, "%s", phase)
		require.Equalf(t, factCommit, gone.Commit, "%s: pinned at the fact's commit", phase)
		require.Truef(t, gone.Deleted, "%s: no file at the pin", phase)
		require.Falsef(t, gone.Superseded, "%s", phase)
		require.Emptyf(t, gone.Blob, "%s: no version to name", phase)
		require.Nilf(t, gone.Content, "%s", phase)

		sib := asFileNode(t, nodes[sysOntology])
		require.Falsef(t, sib.Deleted, "%s: a readable sibling is not deleted", phase)
		require.Lenf(t, sib.Blob, 40, "%s", phase)
	}
	check("absent at pin and tip")

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.RawWriteForTest(context.Background(), "agent/test", later, "now\n", "the file arrives through git")
		require.NoError(t, err)
	}))
	check("absent at pin, present at tip")
}

// TestExplain_SymlinkIsNotAFile: a symlink under .knomit/ pointing at a
// regular file beside it is "not found" when explained directly (by path, and
// by path and commit), a fact citing it shows it deleted in the walk, and the
// learn gate refuses a new ref to it as "does not exist"; the regular file it
// points at is unaffected.
// Sabotage: drop the symlink clause in store.SystemFileAt → explain serves
// the link text, the walk node has a blob, learn lands → red.
func TestExplain_SymlinkIsNotAFile(t *testing.T) {
	ri, ctx := sysFileRepo(t)
	seedSysSymlink(t, ri)
	head := headOf(t, ri, "agent/test")

	for _, args := range []map[string]any{{"file": sysSymlink}, {"file": sysSymlink, "commit": head}} {
		text, isErr := callExplain(t, ctx, args)
		require.Truef(t, isErr, "explain %v: %s", args, text)
		require.Containsf(t, text, sysSymlink+" not found", "explain %v", args)
	}
	require.Equal(t, sysFiles[sysSkill], *explainFileDirect(t, ctx, sysSkill, "").Content)

	writeExplainFact(t, ctx, ri, "kb/reference/link.md", "cites a symlink", 0.9, []string{sysSymlink})
	n := asFileNode(t, explainWalk(t, ctx, "kb/reference/link.md")[sysSymlink])
	require.True(t, n.Deleted)
	require.Empty(t, n.Blob)

	before := headOf(t, ri, "agent/test")
	r := learnSysRefs(t, ctx, []any{sysSymlink})
	require.True(t, r.IsError, resultText(t, r))
	require.Contains(t, resultText(t, r), "cites "+sysSymlink+", which does not exist")
	require.Equal(t, before, headOf(t, ri, "agent/test"), "nothing was written")
}
