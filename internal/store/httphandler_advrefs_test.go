package store

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
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

// headlessServed is a store whose CONFIGURED consensus branch does not exist:
// the origin names "master" while the store holds "main". EnsureLocalUpstream
// cannot repair this — it bootstraps only from refs/remotes/origin/<upstream>,
// equally absent — so it is the residual case after Task 4, and the one a
// misconfigured upstream actually produces.
func headlessServed(t *testing.T) (*httptest.Server, plumbing.Hash) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "main", "agent/host-1"))
	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "agent/host-1", "kb/a.md", testFactBody("a", 0.9, nil), "a", "")
	require.NoError(t, err)
	agentTip, err := svc.Branches().HeadCommit(ctx, "agent/host-1")
	require.NoError(t, err)

	svc.SetOrigin(&Origin{URL: "https://example.invalid/kb.git", Branch: "master"})
	require.Equal(t, "master", svc.UpstreamBranch())
	_, err = svc.Branches().HeadCommit(ctx, "master")
	require.Error(t, err, "the fixture must really lack the configured consensus branch")

	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	return srv, plumbing.NewHash(agentTip)
}

// A repo with no consensus branch is REFUSED, not served headless.
//
// Serving the agent branches under an advertisement with no HEAD made real
// `git clone` print a warning and then exit 0 with an empty working tree — a
// subscriber would take that repo as valid and hold nothing. Both clients that
// matter are exercised here rather than assumed: knomit's own subscriber is a
// go-git client, so a message real git renders nicely proves nothing about it.
func TestGitHandler_MissingUpstreamRefusesBothClients(t *testing.T) {
	t.Run("go-git returns an error, not an empty success", func(t *testing.T) {
		srv, _ := headlessServed(t)
		st := memory.NewStorage()
		_, err := gogit.CloneContext(context.Background(), st, nil, &gogit.CloneOptions{
			URL: srv.URL, Tags: gogit.NoTags,
		})
		require.Error(t, err, "a headless repo must not clone clean")
	})

	t.Run("real git fails non-zero and names the branch", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		srv, _ := headlessServed(t)
		cmd := exec.Command("git", "clone", "-q", srv.URL, filepath.Join(t.TempDir(), "c"))
		out, err := cmd.CombinedOutput()
		require.Error(t, err, "git clone must exit non-zero, got:\n%s", out)
		require.Contains(t, string(out), "master",
			"the client must be told WHICH branch is missing:\n%s", out)
	})

	// A client holding a want from an ADVERTISEMENT IT CACHED EARLIER — before
	// the upstream went missing — must be refused at the fetch too, not served
	// a pack from a repo that has no consensus branch.
	t.Run("a fetch is refused too, not just the advertisement", func(t *testing.T) {
		srv, agentTip := headlessServed(t)
		body := uploadPackRaw(t, srv.URL, agentTip)
		require.Contains(t, body, "ERR ")
		require.Contains(t, body, "master", body)
		require.NotContains(t, body, "PACK", "nothing is served from a headless repo")
	})
}

func TestService_UpstreamBranch_DefaultsToMainWithoutOrigin(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	require.Equal(t, "main", svc.UpstreamBranch())
}

// TestGitHandler_DoesNotAdvertiseExperiments: an experiment is local-only and
// must never appear in the served advertisement, so a peer can neither see it
// nor fetch it.
//
// buildAdvRefs is an ALLOWLIST (upstream + refs/heads/agent/), so this holds
// by construction today — which is exactly why the fixture has to contain a
// real exp/* ref with a real commit on it. Without one the test passes on an
// empty repo and says nothing. The exact line count is the second half: a
// "not contains" alone would still pass if the ref were advertised under some
// other spelling.
func TestGitHandler_DoesNotAdvertiseExperiments(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "main", "agent/host-1"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "agent/host-1", "kb/a.md", testFactBody("a", 0.9, nil), "a", "")
	require.NoError(t, err)

	exp, err := svc.Experiments().OpenExperiment(ctx, "hidden", "", "agent/host-1")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, exp.Branch(), "kb/secret.md", testFactBody("secret", 0.9, nil), "s", "")
	require.NoError(t, err)
	expTip, err := svc.Branches().HeadCommit(ctx, exp.Branch())
	require.NoError(t, err)
	require.NotEmpty(t, expTip, "fixture precondition: the experiment ref really exists")

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	out, err := exec.Command("git", "ls-remote", "--symref", srv.URL).CombinedOutput()
	require.NoError(t, err, string(out))
	s := string(out)
	require.Contains(t, s, "\trefs/heads/agent/host-1\n", "sanity: the agent branch IS advertised")
	require.NotContains(t, s, "refs/heads/exp/", "no experiment ref is advertised")
	require.NotContains(t, s, expTip, "and its tip is not reachable under any other name")
	require.Equal(t, 4, strings.Count(s, "\n"),
		"exactly the symref line, HEAD, main and one agent branch — the experiment adds none:\n%s", s)
}
