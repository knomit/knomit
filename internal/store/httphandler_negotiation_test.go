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
	out, err := exec.Command("git", "clone", "-q", srv.URL, clone).CombinedOutput()
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

	out, err = exec.Command("git", "-C", clone, "pull", "--no-rebase", "--ff-only").CombinedOutput()
	require.NoError(t, err, "incremental pull failed: %s", out)
	require.NotContains(t, string(out), "bad line length",
		"smart-HTTP negotiation produced a corrupt pack stream: %s", out)
}
