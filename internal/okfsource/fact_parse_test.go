package okfsource

import (
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

// A file that meant to be a fact but does not parse is knowledge DROPPED from a
// published base. It was previously indistinguishable from a stray README.
func TestLoad_UnparseableFactIsReported(t *testing.T) {
	r, err := git.Init(memory.NewStorage(), nil)
	require.NoError(t, err)

	head := commitWith(t, r, "learn: seed", "a+learn@agents.knomit.io",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), map[string]string{
			"kb/decisions/x/aaaaaaaa.md": degradationFact("Alpha"),
			// Fact-shaped name, unparseable content: the corruption case.
			"kb/decisions/x/ccccccc1.md": "not a fact at all\n",
			// Fact-shaped content whose YAML is broken: the other corruption case.
			"kb/decisions/x/ccccccc2.md": "---\nkind: [unclosed\n---\n# Broken\n",
		})

	snap, err := Load(r.Storer, head)
	require.NoError(t, err)

	require.Len(t, snap.Facts, 1, "only the parseable fact is exported")
	joined := strings.Join(snap.Warnings, "\n")
	require.Contains(t, joined, "could not be parsed as a fact")
	require.Contains(t, joined, "NOT exported")
	require.Contains(t, joined, "kb/decisions/x/ccccccc1.md")
	require.Contains(t, joined, "kb/decisions/x/ccccccc2.md")
}

// The other half: ordinary markdown under kb/ is not a fact and never was, so
// reporting it would train users to ignore the warning that matters.
func TestLoad_NonFactMarkdownStaysSilent(t *testing.T) {
	r, err := git.Init(memory.NewStorage(), nil)
	require.NoError(t, err)

	head := commitWith(t, r, "learn: seed", "a+learn@agents.knomit.io",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), map[string]string{
			"kb/decisions/x/aaaaaaaa.md": degradationFact("Alpha"),
			"kb/README.md":               "# About this knowledge base\n\nNotes.\n",
			"kb/decisions/notes.md":      "# Scratch\n\nJust a note.\n",
		})

	snap, err := Load(r.Storer, head)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 1)
	require.Empty(t, snap.Warnings, "a README under kb/ is not lost knowledge")
}

func TestLooksLikeFact(t *testing.T) {
	for _, tc := range []struct {
		name, path, content string
		want                bool
	}{
		{"uuid8 basename", "kb/a/b/aaaaaaaa.md", "garbage", true},
		{"frontmatter fence", "kb/a/b/notes.md", "---\nkind: x\n---\n# T\n", true},
		{"plain readme", "kb/README.md", "# About\n", false},
		{"named note", "kb/a/b/scratch.md", "# Scratch\n", false},
		{"uuid-length but not hex", "kb/a/b/zzzzzzzz.md", "# Nope\n", false},
		{"too short", "kb/a/b/abc.md", "# Nope\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, looksLikeFact(tc.path, tc.content))
		})
	}
}
