package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

// servedStore seeds n fact commits on "main" (the served upstream) and serves it.
func servedStore(t *testing.T, n int) (*Service, *httptest.Server) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	for i := range n {
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/f-%02d.md", i),
			testFactBody(fmt.Sprintf("f %d", i), 0.9, nil), fmt.Sprintf("f %d", i), "")
		require.NoError(t, err)
	}
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	return svc, srv
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

// go-git at depth 1 is what the wizard's probe sends. It must get ONE commit
// with a complete tree.
func TestGitHandler_GoGitDepth1CloneIsCompleteAndShallow(t *testing.T) {
	_, srv := servedStore(t, 5)
	st := memory.NewStorage()
	repo, err := gogit.CloneContext(context.Background(), st, nil, &gogit.CloneOptions{
		URL: srv.URL, SingleBranch: true, Depth: 1, Tags: gogit.NoTags,
	})
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	c, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	tree, err := c.Tree()
	require.NoError(t, err)
	_, err = tree.File("kb/f-04.md") // full tree present at the tip
	require.NoError(t, err)
	shallows, err := st.Shallow()
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{head.Hash()}, shallows, "tip is the shallow boundary")
	_, err = c.Parent(0)
	require.Error(t, err, "parent not transferred")
}

// Depth 3 transfers exactly three commits, each with a complete tree, and
// makes the third the boundary. Counted by walking first parents by hand:
// go-git's Log is NOT shallow-aware — it walks past the boundary and fails
// with "object not found" — so a commit count taken through it would be
// measuring the client, not the server.
func TestGitHandler_GoGitDepth3(t *testing.T) {
	_, srv := servedStore(t, 5)
	st := memory.NewStorage()
	repo, err := gogit.CloneContext(context.Background(), st, nil, &gogit.CloneOptions{
		URL: srv.URL, SingleBranch: true, Depth: 3, Tags: gogit.NoTags,
	})
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	h := head.Hash()
	var last *object.Commit
	for i := range 3 {
		c, err := repo.CommitObject(h)
		require.NoError(t, err, "commit %d of 3 not transferred", i+1)
		tree, err := c.Tree()
		require.NoError(t, err)
		_, err = tree.File(fmt.Sprintf("kb/f-%02d.md", 4-i))
		require.NoError(t, err, "commit %d of 3 has an incomplete tree", i+1)
		last = c
		if i < 2 {
			require.Equal(t, 1, c.NumParents())
			h = c.ParentHashes[0]
		}
	}

	shallows, err := st.Shallow()
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{last.Hash}, shallows, "third commit is the boundary")
	require.Equal(t, 1, last.NumParents())
	_, err = repo.CommitObject(last.ParentHashes[0])
	require.Error(t, err, "fourth commit must not be transferred")
}

// A full clone must keep working: the same builder answers a request with no
// depth, and nothing about it is shallow.
func TestGitHandler_GoGitFullCloneUnaffected(t *testing.T) {
	_, srv := servedStore(t, 5)
	st := memory.NewStorage()
	repo, err := gogit.CloneContext(context.Background(), st, nil, &gogit.CloneOptions{
		URL: srv.URL, Tags: gogit.NoTags,
	})
	require.NoError(t, err)
	iter, err := repo.Log(&gogit.LogOptions{})
	require.NoError(t, err)
	n := 0
	require.NoError(t, iter.ForEach(func(*object.Commit) error { n++; return nil }))
	require.Equal(t, 6, n, "root + 5 facts")
	shallows, err := st.Shallow()
	require.NoError(t, err)
	require.Empty(t, shallows)
}

// Real git: shallow clone, then the server advances by more than one have
// batch, then deepen and unshallow. Every step must leave a complete checkout.
func TestGitHandler_RealGitShallowCloneDeepenUnshallow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	svc, srv := servedStore(t, 25)
	work := t.TempDir()
	gitOut(t, "", "clone", "-q", "--depth", "1", srv.URL, work)
	require.Equal(t, "1\n", gitOut(t, work, "rev-list", "--count", "HEAD"))
	require.FileExists(t, filepath.Join(work, "kb/f-24.md"))

	ctx := context.Background()
	for i := 25; i < 45; i++ { // > one 16-have batch of new history
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/f-%02d.md", i),
			testFactBody(fmt.Sprintf("f %d", i), 0.9, nil), fmt.Sprintf("f %d", i), "")
		require.NoError(t, err)
	}
	gitOut(t, work, "pull", "-q", "--ff-only")
	require.FileExists(t, filepath.Join(work, "kb/f-44.md"))
	require.Empty(t, strings.TrimSpace(gitOut(t, work, "fsck", "--connectivity-only")))

	// --depth=N is absolute and needs only the `shallow` capability;
	// --deepen=N is deepen-relative, which this server deliberately does not
	// advertise, so git would refuse it before sending anything.
	gitOut(t, work, "fetch", "-q", "--depth=3")
	require.Equal(t, "3\n", gitOut(t, work, "rev-list", "--count", "HEAD"))

	gitOut(t, work, "fetch", "-q", "--unshallow")
	require.Equal(t, "46\n", gitOut(t, work, "rev-list", "--count", "HEAD")) // root + 45 facts
	require.Empty(t, strings.TrimSpace(gitOut(t, work, "fsck", "--connectivity-only")))
	require.FileExists(t, filepath.Join(work, "kb/f-00.md"))
}

// An incremental fetch negotiates over several rounds, and every round but the
// last is discarded. The object set — a full history walk plus a recursive
// tree walk per commit — must be built ONCE, on the round that actually sends
// a pack.
//
// This regresses a measured defect: the object set used to be built before the
// early NAK returns, so an incremental pull against an 800-commit store did
// seven builds and threw six away (1.1s, against a code path that previously
// returned NAK before touching a single tree). The responses are
// byte-identical either way, so counting the builds is the only way to see it.
func TestGitHandler_BuildsTheObjectSetOncePerFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Enough history that git needs more than one have-batch, which is what
	// creates the discarded rounds in the first place.
	svc, srv := servedStore(t, 60)
	work := t.TempDir()

	gitOut(t, "", "clone", "-q", srv.URL, work)
	require.FileExists(t, filepath.Join(work, "kb/f-59.md"))

	ctx := context.Background()
	for i := 60; i < 100; i++ {
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/f-%02d.md", i),
			testFactBody(fmt.Sprintf("f %d", i), 0.9, nil), fmt.Sprintf("f %d", i), "")
		require.NoError(t, err)
	}

	before := objectBuilds.Load()
	gitOut(t, work, "pull", "-q", "--ff-only")
	builds := objectBuilds.Load() - before

	require.FileExists(t, filepath.Join(work, "kb/f-99.md"), "the pull must still deliver everything")
	require.Equal(t, int64(1), builds,
		"one incremental pull must build the object set exactly once, not once per negotiation round")
}

// The same guarantee for the shape that has no haves at all: a fresh clone is
// one round, so it is one build.
func TestGitHandler_FullCloneBuildsTheObjectSetOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	_, srv := servedStore(t, 20)
	before := objectBuilds.Load()
	gitOut(t, "", "clone", "-q", srv.URL, t.TempDir())
	require.Equal(t, int64(1), objectBuilds.Load()-before)
}

// uploadPackRaw POSTs a hand-built upload-pack request. A real git client
// refuses an unadvertised want BEFORE sending anything (there is no
// client-side override; uploadpack.allowAnySHA1InWant is a server setting),
// so the server's own refusal is only reachable by speaking the protocol
// directly.
func uploadPackRaw(t *testing.T, url string, wants ...plumbing.Hash) string {
	t.Helper()
	var body bytes.Buffer
	enc := pktline.NewEncoder(&body)
	for i, w := range wants {
		if i == 0 {
			require.NoError(t, enc.Encodef("want %s ofs-delta agent=test\n", w.String()))
			continue
		}
		require.NoError(t, enc.Encodef("want %s\n", w.String()))
	}
	require.NoError(t, enc.Flush())
	require.NoError(t, enc.Encodef("done\n"))

	resp, err := http.Post(url+"/git-upload-pack", "application/x-git-upload-pack-request", &body)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(out)
}

// A want that is not an advertised tip is refused with a git ERR line. This is
// what keeps the hidden refs of Task 1 genuinely private: knowing a hidden
// tip's hash must not be enough to fetch it.
func TestGitHandler_RefusesWantOutsideAdvertisedTips(t *testing.T) {
	svc, srv := servedStore(t, 3)
	tip, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)
	tipHash := plumbing.NewHash(tip)
	c, err := svc.rh.repo.CommitObject(tipHash)
	require.NoError(t, err)
	parent, err := c.Parent(0)
	require.NoError(t, err)

	// Positive control FIRST: the identical request shape against the
	// advertised tip is served, so the refusal below is about the want and
	// not about a malformed hand-built request.
	served := uploadPackRaw(t, srv.URL, tipHash)
	require.Contains(t, served, "PACK", "advertised tip must still be served")
	require.NotContains(t, served, "ERR ")

	refused := uploadPackRaw(t, srv.URL, parent.Hash)
	require.Contains(t, refused, "ERR ")
	require.Contains(t, refused, "not an advertised tip", refused)
	require.NotContains(t, refused, "PACK", "nothing is sent for a refused want")
}
