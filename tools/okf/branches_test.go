package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func branches(t *testing.T, outDir string, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, runBranches(args, outDir, &buf))
	return buf.String()
}

// The whole point of the command: show which source branches exist, which are
// exported, and which are not — without the user guessing names.
func TestCLI_BranchesListsExportedAndUnexported(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	// A second source branch that will NOT be exported.
	wt, err := kbRepo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("agent/foobar"), Create: true,
	}))
	kbCommit(t, kbRepo, kbDir, "learn: agent fact", map[string]string{
		"kb/decisions/x/dddddddd.md": factBody("Delta", 0.6),
	}, nil)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}))

	outDir, _ := cloneKB(t, kbDir)
	got := branches(t, outDir)

	require.Contains(t, got, "master", "the exported branch is listed")
	require.Contains(t, got, "up to date")
	require.Contains(t, got, "agent/foobar", "an unexported source branch is still listed")
	require.Contains(t, got, "not exported")
	require.Contains(t, got, "knomit-okf sync -b", "and the command to export one is shown")

	// The checked-out output branch is marked.
	var masterLine string
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "master") && !strings.Contains(l, "BRANCH") {
			masterLine = l
		}
	}
	require.Contains(t, masterLine, "*", "the current output branch is marked: %q", masterLine)
}

// A source branch that has moved since the export must report HOW FAR behind,
// so a user can tell a trivial drift from a stale bundle.
func TestCLI_BranchesReportsCommitsBehind(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	require.Contains(t, branches(t, outDir), "up to date")

	for i, title := range []string{"Gamma", "Delta", "Epsilon"} {
		kbCommit(t, kbRepo, kbDir, "learn: add "+title, map[string]string{
			"kb/decisions/x/" + strings.Repeat(string(rune('c'+i)), 8) + ".md": factBody(title, 0.7),
		}, nil)
	}

	got := branches(t, outDir)
	require.Contains(t, got, "3 commits behind", "got:\n%s", got)

	// After syncing it is current again.
	sync(t, outDir)
	require.Contains(t, branches(t, outDir), "up to date")
}

// --no-fetch must not contact the remote, and must say the counts may be stale
// rather than silently reporting numbers computed from old refs.
func TestCLI_BranchesNoFetchIsOfflineAndSaysSo(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	outDir, _ := cloneKB(t, kbDir)

	// Move the source; --no-fetch must NOT see it.
	kbCommit(t, kbRepo, kbDir, "learn: add gamma", map[string]string{
		"kb/decisions/x/cccccccc.md": factBody("Gamma", 0.7),
	}, nil)

	offline := branches(t, outDir, "--no-fetch")
	require.Contains(t, offline, "may be stale")
	require.Contains(t, offline, "up to date", "without fetching, the stale ref still looks current")

	// With a fetch, the drift shows.
	require.Contains(t, branches(t, outDir), "1 commit behind")
}

// An output branch whose source branch has been deleted upstream must be
// called out rather than silently omitted — it is published knowledge with no
// remaining origin.
func TestCLI_BranchesFlagsAnOutputWhoseSourceIsGone(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	wt, err := kbRepo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("agent/temp"), Create: true,
	}))
	kbCommit(t, kbRepo, kbDir, "learn: temp fact", map[string]string{
		"kb/decisions/x/dddddddd.md": factBody("Delta", 0.6),
	}, nil)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}))

	outDir, _ := cloneKB(t, kbDir)
	sync(t, outDir, "-b", "agent/temp") // export it
	require.Contains(t, branches(t, outDir), "agent/temp")

	// The source branch disappears upstream.
	require.NoError(t, kbRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("agent/temp")))

	got := branches(t, outDir)
	require.Contains(t, got, "source branch gone",
		"an export with no upstream must be flagged; got:\n%s", got)
}

// Reading each branch's config from its committed TREE is what lets the
// command describe branches that are not checked out. If it read the working
// tree instead, every branch but one would look unexported.
func TestCLI_BranchesReadsConfigWithoutCheckout(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	wt, err := kbRepo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("agent/other"), Create: true,
	}))
	kbCommit(t, kbRepo, kbDir, "learn: other", map[string]string{
		"kb/decisions/x/dddddddd.md": factBody("Delta", 0.6),
	}, nil)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}))

	outDir, _ := cloneKB(t, kbDir)
	sync(t, outDir, "-b", "agent/other") // leaves agent/other checked out

	got := branches(t, outDir)
	// BOTH are exported and neither says "not exported", even though only one
	// is in the working tree.
	require.NotContains(t, got, "not exported", "got:\n%s", got)
	require.Contains(t, got, "master")
	require.Contains(t, got, "agent/other")
}
