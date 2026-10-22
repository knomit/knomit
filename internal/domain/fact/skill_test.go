package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSkill_Valid(t *testing.T) {
	sk, err := ParseSkill("work-task", []byte("---\nname: work-task\ndescription: Work one task.\nlicense: MIT\n---\n\n# Work\n\nDo it.\n"))
	require.NoError(t, err)
	require.Equal(t, Skill{Name: "work-task", Description: "Work one task.", Body: "# Work\n\nDo it.\n"}, sk)

	// CRLF and a BOM are what an editor on Windows writes.
	sk, err = ParseSkill("a", []byte("\xEF\xBB\xBF---\r\nname: a\r\ndescription: d\r\n---\r\nbody\r\n"))
	require.NoError(t, err)
	require.Equal(t, "body\n", sk.Body)

	// Frontmatter closed at end of file: an empty body is allowed.
	sk, err = ParseSkill("a", []byte("---\nname: a\ndescription: d\n---"))
	require.NoError(t, err)
	require.Equal(t, "", sk.Body)
}

func TestParseSkill_Malformed(t *testing.T) {
	for _, tc := range []struct{ dir, src, want string }{
		{"a", "# no frontmatter\n", "no YAML frontmatter"},
		{"a", "---\nname: a\ndescription: d\n", "not closed"},
		{"a", "---\nname: [a\n---\n", "frontmatter"},
		{"a", "---\ndescription: d\n---\n", "no name"},
		{"Bad_Name", "---\nname: Bad_Name\ndescription: d\n---\n", "kebab-case"},
		{"a", "---\nname: b\ndescription: d\n---\n", `differs from its folder "a"`},
		{"a", "---\nname: a\n---\n", "no description"},
	} {
		_, err := ParseSkill(tc.dir, []byte(tc.src))
		require.Error(t, err, tc.src)
		require.Contains(t, err.Error(), tc.want, tc.src)
	}
}

func TestApplySkillArguments(t *testing.T) {
	require.Equal(t, "Do task-7 now, task-7.", ApplySkillArguments("Do $ARGUMENTS now, $ARGUMENTS.", " task-7 "))
	require.Equal(t, "Do it.\n\nArguments: task-7\n", ApplySkillArguments("Do it.\n", "task-7"))
	require.Equal(t, "Do  now.", ApplySkillArguments("Do $ARGUMENTS now.", ""))
	require.Equal(t, "Do it.\n", ApplySkillArguments("Do it.\n", ""))
}

func TestSkillPath(t *testing.T) {
	require.Equal(t, ".knomit/skills/work-task/SKILL.md", SkillPath("work-task"))
	require.True(t, ValidSkillName("work-task"))
	require.False(t, ValidSkillName("../x"))
	require.False(t, ValidSkillName("Work"))
}
