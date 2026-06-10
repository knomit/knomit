package store

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSmartHTTP_IncrementalFetchMultiRoundNegotiation regresses the
// "fatal: protocol error: bad line length character: PACK" failure seen when
// cloning a knomit-served repo and then pulling newly-added commits.
//
// The trigger is multi-round stateless-RPC negotiation: git sends its first
// batch of ~16 "have" lines WITHOUT a trailing "done", expecting only an
// ACK/NAK section in response. A naive upload-pack server that appends the
// packfile on every POST corrupts the stream — the client reads the raw
// "PACK" bytes where it expects the next pkt-line.
//
// To force multiple negotiation rounds the clone must carry more than one
// have-batch of history, so we seed >16 commits before cloning and then
// advance the branch before pulling.
func TestSmartHTTP_IncrementalFetchMultiRoundNegotiation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	const branch = "main"

	// Seed >16 commits so a later clone has enough history to offer multiple
	// batches of "have" lines, pushing git into multi-round negotiation.
	for i := range 25 {
		_, err := svc.Facts().WriteFact(ctx, branch,
			fmt.Sprintf("kb/seed-%02d.md", i),
			testFactBody(fmt.Sprintf("seed %d", i), 0.9, nil),
			fmt.Sprintf("seed %d", i), "")
		require.NoError(t, err)
	}

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	clone := t.TempDir()
	out, err := exec.Command("git",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"clone", "-q", srv.URL, clone).CombinedOutput()
	require.NoError(t, err, "clone failed: %s", out)

	// Advance the served branch after the clone so the pull is a genuine
	// incremental fetch rather than a no-op.
	for i := range 5 {
		_, err := svc.Facts().WriteFact(ctx, branch,
			fmt.Sprintf("kb/post-%02d.md", i),
			testFactBody(fmt.Sprintf("post %d", i), 0.9, nil),
			fmt.Sprintf("post %d", i), "")
		require.NoError(t, err)
	}

	out, err = exec.Command("git", "-C", clone,
		"-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"pull", "--no-rebase", "--ff-only").CombinedOutput()
	require.NoError(t, err, "incremental pull failed: %s", out)
	require.NotContains(t, string(out), "bad line length",
		"smart-HTTP negotiation produced a corrupt pack stream: %s", out)
}

// TestSmartHTTP_DivergentCloneNegotiationNAKRound hammers the NAK-only
// negotiation branch (`!requestHasDone && !haveCommon`): the server must reply
// with an ACK/NAK section ONLY — here NAK — and stream nothing, letting the
// client come back with an older batch of haves.
//
// The incremental-fetch test above already crosses this branch, but only on a
// round whose haves the server happens not to hold yet. This test forces the
// stronger case: >16 DIVERGENT local commits piled onto the clone, so git's
// commit-date-ordered negotiation offers a whole opening have-batch of commits
// the server has NEVER seen (newest-first, pinned here via far-future commit
// dates). That makes `firstCommonHave` return false on a real, non-empty batch
// and forces multiple NAK rounds before a later batch reaches the shared
// history and converges — exercising the multi-round loop, not just a single
// trivial NAK.
func TestSmartHTTP_DivergentCloneNegotiationNAKRound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	const branch = "main"

	for i := range 25 {
		_, err := svc.Facts().WriteFact(ctx, branch,
			fmt.Sprintf("kb/seed-%02d.md", i),
			testFactBody(fmt.Sprintf("seed %d", i), 0.9, nil),
			fmt.Sprintf("seed %d", i), "")
		require.NoError(t, err)
	}

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	clone := t.TempDir()
	out, err := exec.Command("git",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"clone", "-q", srv.URL, clone).CombinedOutput()
	require.NoError(t, err, "clone failed: %s", out)

	// Pile >16 divergent local commits onto the clone with far-future dates, so
	// git's commit-date-ordered negotiation offers them ahead of the shared
	// history and the server's first have-batch contains nothing it holds.
	for i := range 20 {
		cmd := exec.Command("git", "-C", clone,
			"-c", "user.email=test@knomit.test",
			"-c", "user.name=knomit-test",
			"-c", "commit.gpgsign=false",
			"-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"commit", "--allow-empty", "-q", "-m",
			fmt.Sprintf("divergent local %d", i))
		// Unix epoch for 2100-01-01, +1 min per commit so they stay ordered.
		date := fmt.Sprintf("@%d +0000", 4102444800+i*60)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "local commit failed: %s", out)
	}

	// Advance the served branch so the fetch has real objects to transfer.
	for i := range 5 {
		_, err := svc.Facts().WriteFact(ctx, branch,
			fmt.Sprintf("kb/post-%02d.md", i),
			testFactBody(fmt.Sprintf("post %d", i), 0.9, nil),
			fmt.Sprintf("post %d", i), "")
		require.NoError(t, err)
	}

	// The first have-batch (the divergent locals) draws a NAK; only a later
	// batch reaching the shared history converges. The fetch must complete
	// cleanly with no corrupt pack stream.
	out, err = exec.Command("git", "-C", clone,
		"-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"fetch", "-q", "origin").CombinedOutput()
	require.NoError(t, err, "fetch after divergence failed: %s", out)
	require.NotContains(t, string(out), "bad line length",
		"smart-HTTP negotiation produced a corrupt pack stream: %s", out)
}
