package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

const pushTestFP = "abcd1234" + "00000000000000000000000000000000000000000000000000000000"

// gitMount is /git as server.go mounts it, behind a fixed principal: the
// edge's answer (AuthMiddleware) stands in as a constant so each principal
// kind can be tried, and everything after it — writeGate, pushPermission,
// GitRemoteHandler, the store — is the production chain.
func gitMount(t *testing.T, m *repos.Manager, p auth.Principal) *httptest.Server {
	t.Helper()
	g := (&Server{Auth: config.AuthConfig{}}).grants()
	h := writeGate(g, false)(pushPermission(g)(GitRemoteHandler(m)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// hostWithRepo is one instance with repo "kb" whose main holds one fact.
func hostWithRepo(t *testing.T) (*repos.Manager, *repos.RepoInstance) {
	t.Helper()
	ctx := context.Background()
	m := newPeerManager(t, "agent/host-11111111")
	ri, err := m.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	require.NoError(t, err)
	require.NoError(t, ri.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteFact(ctx, "agent/host-11111111", "kb/first.md", originFactBody("first"), "first", "")
		require.NoError(t, werr)
		_, rerr := s.AdvanceLocalUpstream(ctx, "agent/host-11111111", "main")
		require.NoError(t, rerr)
	}))
	return m, ri
}

// pushOneCommit clones url, adds one file on top of main and pushes it to
// dst. Returns the pushed commit and the push error.
func pushOneCommit(t *testing.T, url, dst string) (plumbing.Hash, error) {
	t.Helper()
	repo, err := gogit.Clone(memory.NewStorage(), nil, &gogit.CloneOptions{URL: url})
	require.NoError(t, err)
	head, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true)
	require.NoError(t, err)
	parent, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	st := repo.Storer

	blob := st.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, _ := blob.Writer()
	_, _ = w.Write([]byte(originFactBody("pushed")))
	_ = w.Close()
	bh, err := st.SetEncodedObject(blob)
	require.NoError(t, err)
	ptree, err := parent.Tree()
	require.NoError(t, err)
	entries := append([]object.TreeEntry{}, ptree.Entries...)
	entries = append(entries, object.TreeEntry{Name: "pushed.md", Mode: filemode.Regular, Hash: bh})
	// kb/ is a subtree; put the file at the root of the tree, which is enough
	// for a push test (the tree is valid git either way).
	to := st.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: sortEntries(entries)}).Encode(to))
	th, err := st.SetEncodedObject(to)
	require.NoError(t, err)
	when := time.Unix(1790000000, 0).UTC()
	c := &object.Commit{
		Author:    object.Signature{Name: "peer", Email: "peer-abcd1234+learn@agents.knomit.io", When: when},
		Committer: object.Signature{Name: "peer", Email: "peer-abcd1234@agents.knomit.io", When: when},
		Message:   "pushed", TreeHash: th, ParentHashes: []plumbing.Hash{parent.Hash},
	}
	co := st.NewEncodedObject()
	require.NoError(t, c.Encode(co))
	ch, err := st.SetEncodedObject(co)
	require.NoError(t, err)
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/work", ch)))
	_, err = repo.CreateRemote(&gogitconfig.RemoteConfig{Name: "dst", URLs: []string{url}})
	require.NoError(t, err)
	return ch, repo.Push(&gogit.PushOptions{RemoteName: "dst", Progress: &bytes.Buffer{},
		RefSpecs: []gogitconfig.RefSpec{gogitconfig.RefSpec("+refs/heads/work:" + dst)}})
}

func sortEntries(es []object.TreeEntry) []object.TreeEntry {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j].Name < es[j-1].Name; j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
	return es
}

func refOf(t *testing.T, ri *repos.RepoInstance, branch string) plumbing.Hash {
	t.Helper()
	h := plumbing.ZeroHash
	require.NoError(t, ri.WithRead(func(s *store.Service) {
		if c, err := s.Branches().HeadCommit(context.Background(), branch); err == nil {
			h = plumbing.NewHash(c)
		}
	}))
	return h
}

func rawReceivePack(t *testing.T, url string, cmd *packp.Command) string {
	t.Helper()
	req := packp.NewReferenceUpdateRequest()
	require.NoError(t, req.Capabilities.Set(capability.ReportStatus))
	req.Commands = []*packp.Command{cmd}
	var buf bytes.Buffer
	require.NoError(t, req.Encode(&buf))
	resp, err := http.Post(url+"/git-receive-pack", "application/x-git-receive-pack-request", &buf)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// T10: only an instance certificate may push, and it needs push:own, not
// write — the write gate exempts receive-pack.
func TestGitPush_OnlyInstanceCertificatesPush(t *testing.T) {
	m, ri := hostWithRepo(t)
	peer := "refs/heads/agent/peer-abcd1234"

	for _, p := range []auth.Principal{
		{Kind: auth.KindAnonymous, Via: auth.ViaNone}, // holds push:own via loopback_default, has no branch
		{Kind: auth.KindBridge, ID: "uid:501", Via: auth.ViaSocket},
		auth.OperatorPrincipal(pushTestFP),
	} {
		t.Run(p.String(), func(t *testing.T) {
			srv := gitMount(t, m, p)
			_, err := pushOneCommit(t, srv.URL+"/kb", peer)
			require.Error(t, err)
			require.Contains(t, err.Error(), "pushing requires an enrolled instance certificate; principal "+p.String()+" is not one")
			out := rawReceivePack(t, srv.URL+"/kb", &packp.Command{Name: plumbing.ReferenceName(peer), New: refOf(t, ri, "main")})
			require.Contains(t, out, "pushing requires an enrolled instance certificate")
			require.Equal(t, plumbing.ZeroHash, refOf(t, ri, "agent/peer-abcd1234"))
		})
	}

	t.Run("instance certificate without write", func(t *testing.T) {
		srv := gitMount(t, m, auth.InstancePrincipal(pushTestFP))
		tip, err := pushOneCommit(t, srv.URL+"/kb", peer)
		require.NoError(t, err)
		require.Equal(t, tip, refOf(t, ri, "agent/peer-abcd1234"))
	})
}

// T11: a subscription accepts no pushes, from anyone.
func TestGitPush_SubscriptionRefuses(t *testing.T) {
	ctx := context.Background()
	a, _ := hostWithRepo(t)
	srvA := httptest.NewServer(GitRemoteHandler(a))
	t.Cleanup(srvA.Close)
	b := newPeerManager(t, "agent/b-22222222")
	riB, err := b.Create(ctx, repos.CreateSpec{Name: "sub", Mode: "subscribe", Origin: &repos.OriginSpec{URL: srvA.URL + "/kb"}}, nil)
	require.NoError(t, err)
	require.True(t, riB.Subscribed())

	srv := gitMount(t, b, auth.InstancePrincipal(pushTestFP))
	_, err = pushOneCommit(t, srv.URL+"/sub", "refs/heads/agent/peer-abcd1234")
	require.Error(t, err)
	require.Contains(t, err.Error(), `repo "sub" is a subscription; it accepts no pushes`)
	require.Equal(t, plumbing.ZeroHash, refOf(t, riB, "agent/peer-abcd1234"))
}
