package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// putRaw commits path EXACTLY as spelled, as a git commit would (WriteFact
// lowercases every nested path, so it cannot write SKILL.md).
func putRaw(t *testing.T, svc *Service, branch, path, content string) plumbing.Hash {
	t.Helper()
	c, _, err := svc.fi.writeFileExact(context.Background(), branch, path, content, "w "+path, "updated", "")
	require.NoError(t, err)
	return plumbing.NewHash(c)
}

// SkillsAt reads the COMMIT it is given: a folder with a SKILL.md is a skill,
// one without is not, a loose file is not; bundled files are every other file
// under the folder, recursively, sorted, SKILL.md excluded.
func TestSkillsAt_ReadsTheCommitsOwnTree(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()

	root, err := svc.Triggers().UpstreamTip(ctx, "main")
	require.NoError(t, err)
	got, err := svc.Skills().SkillsAt(ctx, root)
	require.NoError(t, err)
	require.Empty(t, got, "no .knomit/skills folder is an empty list, not an error")

	// knomit's own write doors lowercase nested paths, so a skill written
	// through knomit lands as skill.md: it is the same skill.
	_, err = svc.Facts().WriteFact(ctx, "main", ".knomit/skills/zeta/SKILL.md", "Z", "w", "updated")
	require.NoError(t, err)
	putRaw(t, svc, "main", ".knomit/skills/alpha/SKILL.md", "A")
	putRaw(t, svc, "main", ".knomit/skills/alpha/ref.md", "R")
	putRaw(t, svc, "main", ".knomit/skills/alpha/scripts/run.sh", "S")
	putRaw(t, svc, "main", ".knomit/skills/no-skill-md/notes.md", "N")
	tip := putRaw(t, svc, "main", ".knomit/skills/loose.md", "L")
	agent := putRaw(t, svc, "agent/a", ".knomit/skills/agent-only/SKILL.md", "X")

	got, err = svc.Skills().SkillsAt(ctx, tip)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "alpha", got[0].Dir)
	require.Equal(t, "A", string(got[0].Data))
	require.NotEmpty(t, got[0].Blob)
	require.Equal(t, "zeta", got[1].Dir)
	require.Equal(t, "Z", string(got[1].Data))

	files, err := svc.Skills().SkillFilesAt(ctx, tip, "alpha")
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.Equal(t, "ref.md", files[0].Path)
	require.Equal(t, "scripts/run.sh", files[1].Path)
	require.EqualValues(t, 1, files[1].Size)
	data, err := files[1].Contents()
	require.NoError(t, err)
	require.Equal(t, "S", string(data))

	lower, err := svc.Skills().SkillFilesAt(ctx, tip, "zeta")
	require.NoError(t, err)
	require.Empty(t, lower, "a lowercase skill.md is the skill file, not a bundled file")

	none, err := svc.Skills().SkillFilesAt(ctx, tip, "absent")
	require.NoError(t, err)
	require.Empty(t, none)
	_, err = svc.Skills().SkillFilesAt(ctx, tip, "../x")
	require.Error(t, err)

	// The agent branch's commit has its own tree: the reader never mixes.
	onAgent, err := svc.Skills().SkillsAt(ctx, agent)
	require.NoError(t, err)
	require.Len(t, onAgent, 1)
	require.Equal(t, "agent-only", onAgent[0].Dir)
}

// TestSkillsAt_SymlinkIsNotAFile (user ruling 2026-10-08: "we do NOT want to
// follow symlinks, so 404"; "small PR for SKILL.md symlink"): a SKILL.md that
// is a symlink (here to a regular file beside it) makes no skill, and a
// regular skill.md beside such a symlink is still the skill. A symlinked
// bundled file is not listed, while the regular file it points at is.
// Sabotage: skillFileEntry or SkillFilesAt back on Mode.IsFile → the linked
// skill is listed, or link.md is bundled → red.
func TestSkillsAt_SymlinkIsNotAFile(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	putRaw(t, svc, "main", ".knomit/skills/linked/body.md", "---\nname: linked\n---\nB")
	putRaw(t, svc, "main", ".knomit/skills/shadow/skill.md", "REGULAR")
	putRaw(t, svc, "main", ".knomit/skills/real/SKILL.md", "R")
	putRaw(t, svc, "main", ".knomit/skills/real/ref.md", "REF")
	link := func(path, target string) plumbing.Hash {
		h, err := svc.RawSymlinkForTest(ctx, "main", path, target, "symlink "+path)
		require.NoError(t, err)
		return plumbing.NewHash(h)
	}
	link(".knomit/skills/linked/SKILL.md", "body.md")
	link(".knomit/skills/shadow/SKILL.md", "skill.md")
	tip := link(".knomit/skills/real/link.md", "ref.md")

	got, err := svc.Skills().SkillsAt(ctx, tip)
	require.NoError(t, err)
	dirs := make([]string, len(got))
	for i, e := range got {
		dirs[i] = e.Dir
	}
	require.Equal(t, []string{"real", "shadow"}, dirs, "a symlinked SKILL.md is no skill")
	require.Equal(t, "REGULAR", string(got[1].Data), "the regular skill.md, not the symlink's target name")

	files, err := svc.Skills().SkillFilesAt(ctx, tip, "real")
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "ref.md", files[0].Path, "the symlinked link.md is not a bundled file")

	shadow, err := svc.Skills().SkillFilesAt(ctx, tip, "shadow")
	require.NoError(t, err)
	require.Empty(t, shadow, "the skill.md is the skill file, and the SKILL.md symlink is not a file")
}
