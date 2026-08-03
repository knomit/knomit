package source

import (
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

// The walk bound is the one degradation that gets WORSE with time: commits
// accrue at the tip and push older ones off the back, so a date already
// published can change on a later sync. Silence about it is what these pin.

func degradationFact(title string) string {
	return "---\nkind: epistemic\ntype: observation\ndomain: [x]\nconfidence: 0.9\n---\n# " + title + "\n\nBody.\n"
}

// withMaxCommits lowers the walk bound for one test. Reaching 5000 commits for
// real would dominate the suite's runtime.
func withMaxCommits(t *testing.T, n int) {
	t.Helper()
	prev := maxCommits
	maxCommits = n
	t.Cleanup(func() { maxCommits = prev })
}

// A chain longer than the bound must SAY it was cut. Everything downstream —
// creation dates, per-fact History, views/retired.md — silently loses content
// past that point, and none of the loss is visible in the bundle it corrupts.
func TestLoad_TruncatedHistoryIsReported(t *testing.T) {
	r, err := git.Init(memory.NewStorage(), nil)
	require.NoError(t, err)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Commit 1 is the ONLY one that touches alpha, so once the walk stops short
	// of it, alpha has no authoring time left anywhere in the result.
	files := map[string]string{"kb/decisions/x/aaaaaaaa.md": degradationFact("Alpha")}
	head := commitWith(t, r, "learn: alpha", "a+learn@agents.knomit.io", base, files)
	for i := range 4 {
		files["kb/decisions/x/bbbbbbbb.md"] = degradationFact("Beta " + strings.Repeat("!", i+1))
		head = commitWith(t, r, "learn: beta", "a+learn@agents.knomit.io",
			base.Add(time.Duration(i+1)*time.Hour), files, head)
	}

	withMaxCommits(t, 3) // 5 commits exist, so the walk is genuinely cut

	snap, err := Load(r.Storer, head)
	require.NoError(t, err)

	require.NotEmpty(t, snap.Warnings, "a truncated walk must not be silent")
	joined := strings.Join(snap.Warnings, "\n")
	require.Contains(t, joined, "history walk stopped at the 3-commit bound")
	// The consequence a reader can act on, not merely the fact of truncation:
	// alpha's creation fell off the walk and now carries the export's date.
	// Reported as its own warning, counted, because the bound is a cause and
	// this is the damage.
	require.Contains(t, joined, "1 fact carries the export commit's date rather than its own")

	var alpha string
	for _, f := range snap.Facts {
		if f.Fact.Title == "Alpha" {
			alpha = f.Timestamp.UTC().Format(time.RFC3339)
		}
	}
	require.NotEmpty(t, alpha, "alpha is still in the tree, so it is still exported")
	require.NotEqual(t, base.UTC().Format(time.RFC3339), alpha,
		"this is the corruption being reported: alpha's true date is gone")
}

// The guard against a warning that is always on. A history inside the bound is
// complete, so it must report nothing at all — otherwise every clean run cries
// wolf and the real one stops being read.
func TestLoad_HistoryWithinTheBoundIsSilent(t *testing.T) {
	r, err := git.Init(memory.NewStorage(), nil)
	require.NoError(t, err)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	files := map[string]string{"kb/decisions/x/aaaaaaaa.md": degradationFact("Alpha")}
	head := commitWith(t, r, "learn: alpha", "a+learn@agents.knomit.io", base, files)

	withMaxCommits(t, 50)

	snap, err := Load(r.Storer, head)
	require.NoError(t, err)
	require.Empty(t, snap.Warnings)
	require.Len(t, snap.Facts, 1)
	require.Equal(t, base.UTC(), snap.Facts[0].Timestamp.UTC(),
		"an untruncated walk dates a fact from its own commit")
}

// Load must keep working against a storer with no history at all beyond the
// root, which is the shape every fresh knowledge base starts in.
func TestLoad_RootCommitOnlyNeedsNoWarning(t *testing.T) {
	r, err := git.Init(memory.NewStorage(), nil)
	require.NoError(t, err)
	head := commitWith(t, r, "learn: seed", "a+learn@agents.knomit.io",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		map[string]string{"kb/decisions/x/aaaaaaaa.md": degradationFact("Alpha")})

	snap, err := Load(r.Storer, head)
	require.NoError(t, err)
	require.Empty(t, snap.Warnings)
	require.Equal(t, head, snap.SourceSHA)
}
