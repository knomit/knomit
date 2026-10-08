package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// A symlink under .knomit/skills/ is never followed (user ruling 2026-10-08:
// "we do NOT want to follow symlinks, so 404"; "small PR for SKILL.md
// symlink"): a skill whose SKILL.md is a symlink is absent at every door that
// loads skills (prompts/list, prompts/get, knomit_skill list and get), and a
// symlinked bundled file is neither inlined nor listed. Regular files beside
// them are served as before.

// putSymlink commits a symlink entry in skill name's folder on the repo's
// consensus branch, as a git push would.
func putSymlink(t *testing.T, ri *repos.RepoInstance, name, file, target string) {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	_, err = svc.RawSymlinkForTest(context.Background(), svc.UpstreamBranch(),
		fact.SkillsDir+"/"+name+"/"+file, target, "symlink "+name+"/"+file)
	require.NoError(t, err)
}

// TestSkills_SymlinkedSkillMDIsAbsent: "real" is a regular skill. Two folders
// have a SKILL.md that is a symlink:
//   - "linked" points at a regular, well-formed skill file beside it (the
//     realistic case);
//   - "crafted" has a link TARGET that is itself a well-formed SKILL.md.
//     A link's blob is its target text, so this is the one shape in which a
//     loader reading symlink blobs would pass the parser and LIST the skill.
//
// Every door serves "real" and treats both as no skill at all.
// Sabotage: store.skillFileEntry back on Mode.IsFile → both folders are
// skills again ("crafted" is listed; "linked" is a malformed skill, not "no
// skill") → red at each door.
func TestSkills_SymlinkedSkillMDIsAbsent(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "real", "Real one.", "REAL body\n")
	putFile(t, f.alpha, "", "linked", "body.md", skillMD("linked", "Linked one.", "LINKED body\n"))
	putSymlink(t, f.alpha, "linked", fact.SkillFileName, "body.md")
	putSymlink(t, f.alpha, "crafted", fact.SkillFileName, skillMD("crafted", "Crafted one.", "CRAFTED body\n"))
	srv := NewServer("kb", f.m, false, nil)
	ctx := repoCtx(f.alpha)

	// One subtest per door, so a regression shows which doors it reaches.
	t.Run("prompts/list", func(t *testing.T) {
		ps, _ := listPrompts(t, srv, ctx)
		require.Equal(t, []string{"real"}, promptNames(ps))
	})
	t.Run("prompts/get", func(t *testing.T) {
		for _, name := range []string{"linked", "crafted"} {
			_, errMsg := getPrompt(t, srv, ctx, name, nil)
			require.Containsf(t, errMsg, "no skill "+name+" in repo alpha", "prompts/get %s", name)
		}
		got, errMsg := getPrompt(t, srv, ctx, "real", nil)
		require.Empty(t, errMsg)
		require.Equal(t, "REAL body\n", got.Messages[0].Content.Text)
	})
	t.Run("knomit_skill list", func(t *testing.T) {
		text, isErr := callSkill(t, srv, ctx, `{}`)
		require.False(t, isErr, text)
		var list skillListResponse
		require.NoError(t, json.Unmarshal([]byte(text), &list))
		require.Equal(t, []skillListEntry{{Name: "real", Description: "Real one."}}, list.Skills)
	})
	t.Run("knomit_skill get", func(t *testing.T) {
		for _, name := range []string{"linked", "crafted"} {
			text, isErr := callSkill(t, srv, ctx, `{"name":"`+name+`"}`)
			require.Truef(t, isErr, "knomit_skill get %s: %s", name, text)
			require.Containsf(t, text, "no skill "+name+" in repo alpha", "knomit_skill get %s", name)
		}
		text, isErr := callSkill(t, srv, ctx, `{"name":"real"}`)
		require.False(t, isErr, text)
	})
}

// TestSkills_SymlinkedBundledFileIsAbsent: a skill's bundled file that is a
// symlink (to a regular file beside it) is neither inlined nor listed by
// prompts/get or knomit_skill get; the regular file it points at is inlined.
// Sabotage: store.SkillFilesAt back on Mode.IsFile → link.md is inlined with
// its target's name as text → red.
func TestSkills_SymlinkedBundledFileIsAbsent(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "pack", "Has files.", "Use ref.md.\n")
	putFile(t, f.alpha, "", "pack", "ref.md", "REF\n")
	putSymlink(t, f.alpha, "pack", "link.md", "ref.md")
	srv := NewServer("kb", f.m, false, nil)
	ctx := repoCtx(f.alpha)

	t.Run("prompts/get", func(t *testing.T) {
		got, errMsg := getPrompt(t, srv, ctx, "pack", nil)
		require.Empty(t, errMsg)
		require.Len(t, got.Messages, 2, "body and ref.md only")
		require.Equal(t, "knomit://alpha/.knomit/skills/pack/ref.md", got.Messages[1].Content.Resource.URI)
		for _, m := range got.Messages {
			require.NotContains(t, m.Content.Resource.URI, "link.md")
			require.NotContains(t, m.Content.Text, "link.md")
		}
	})
	t.Run("knomit_skill get", func(t *testing.T) {
		text, isErr := callSkill(t, srv, ctx, `{"name":"pack"}`)
		require.False(t, isErr, text)
		var out skillGetResponse
		require.NoError(t, json.Unmarshal([]byte(text), &out))
		require.Len(t, out.Files, 1)
		require.Equal(t, "ref.md", out.Files[0].Path)
		require.Empty(t, out.NotInlined, "the symlink is not even listed")
	})
}
