package store

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// The served advertisement is a VIEW: HEAD on the consensus branch, only the
// upstream and agent/* heads listed, remote-tracking and refs/knomit hidden.
func TestGitHandler_AdvertisesCuratedView(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "main", "agent/host-1"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "agent/host-1", "kb/a.md", testFactBody("a", 0.9, nil), "a", "")
	require.NoError(t, err)
	tip, err := svc.Branches().HeadCommit(ctx, "agent/host-1")
	require.NoError(t, err)

	// Private refs that must NOT be advertised.
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/remotes/origin/main"), plumbing.NewHash(tip))))
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/knomit/agent-base/agent/host-1"), plumbing.NewHash(tip))))

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	out, err := exec.Command("git", "ls-remote", "--symref", srv.URL).CombinedOutput()
	require.NoError(t, err, string(out))
	s := string(out)
	require.Contains(t, s, "ref: refs/heads/main\tHEAD")
	require.Contains(t, s, "\trefs/heads/main\n")
	require.Contains(t, s, "\trefs/heads/agent/host-1\n")
	require.NotContains(t, s, "refs/remotes/")
	require.NotContains(t, s, "refs/knomit/")
	require.Equal(t, 4, strings.Count(s, "\n"),
		"exactly the symref line, HEAD, main and one agent branch:\n%s", s)
}

// The advertised HEAD follows the CONFIGURED upstream, not the branch the
// store's own git HEAD happens to sit on: an origin-backed repo whose
// remotes row names "master" must serve refs/heads/master as HEAD.
func TestGitHandler_AdvertisedHeadFollowsConfiguredUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "master", "agent/host-1"))
	// Connection identity — including the consensus branch name — is INJECTED
	// from control.db, not read out of the repo's own tables.
	svc.SetOrigin(&Origin{URL: "https://example.invalid/kb.git", Branch: "master"})
	require.Equal(t, "master", svc.UpstreamBranch())

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	out, err := exec.Command("git", "ls-remote", "--symref", srv.URL).CombinedOutput()
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "ref: refs/heads/master\tHEAD")
}

func TestService_UpstreamBranch_DefaultsToMainWithoutOrigin(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	require.Equal(t, "main", svc.UpstreamBranch())
}
