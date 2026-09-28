package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
)

// The pusher every test pushes as. Its fp8 names its branch; the host's own
// branch deliberately shares that fp8, so the host-own-branch refusal is
// reachable (the fp8 rule would otherwise catch it first).
const (
	pushFP     = "abcd1234" + "00000000000000000000000000000000000000000000000000000000"
	peerBranch = "agent/peer-abcd1234"
	hostBranch = "agent/host-abcd1234"
)

// pushHost is a knomit store serving /git with a policy the test controls.
type pushHost struct {
	t   *testing.T
	svc *Service
	srv *httptest.Server
	mu  sync.Mutex
	pol PushPolicy
	// cancelOnRegister, when set, cancels the request's own context at the
	// point register reaches (pushRegisterHook) — a client that went away.
	cancelOnRegister bool
	membersCalls     atomic.Int32
}

func (h *pushHost) setPolicy(p PushPolicy) { h.mu.Lock(); h.pol = p; h.mu.Unlock() }

func newPushHost(t *testing.T, mode string, facts int) *pushHost {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "host.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{fact.OntologyFile: kbOntology(mode)}, hostBranch))
	ctx := context.Background()
	for i := range facts {
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/notes/host-%02d.md", i),
			testFactBody(fmt.Sprintf("host %d", i), 0.9, nil), "h", "")
		require.NoError(t, err)
	}
	h := &pushHost{t: t, svc: svc}
	h.pol = PushPolicy{Pusher: pushFP, OwnBranch: hostBranch}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		pol, cancelReg := h.pol, h.cancelOnRegister
		h.mu.Unlock()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		if cancelReg {
			pushRegisterHook = cancel
			defer func() { pushRegisterHook = nil }()
		}
		svc.Handler().ServeHTTP(w, r.WithContext(WithPushPolicy(ctx, pol)))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// members makes the policy's fleet answer ms (counting calls).
func (h *pushHost) members(ms []FleetMember, err error) func(context.Context) ([]FleetMember, error) {
	return func(context.Context) ([]FleetMember, error) {
		h.membersCalls.Add(1)
		return ms, err
	}
}

func (h *pushHost) objects() int {
	h.t.Helper()
	var n int
	require.NoError(h.t, h.svc.rh.db.QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&n))
	return n
}

func (h *pushHost) ref(branch string) plumbing.Hash {
	h.t.Helper()
	r, err := h.svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return plumbing.ZeroHash
	}
	return r.Hash()
}

// peer is a go-git clone of the host, building commits by hand.
type peer struct {
	t    *testing.T
	repo *gogit.Repository
	n    int
}

func newPeer(t *testing.T, url string) *peer {
	t.Helper()
	repo, err := gogit.Clone(memory.NewStorage(), nil, &gogit.CloneOptions{URL: url})
	require.NoError(t, err)
	return &peer{t: t, repo: repo}
}

func (p *peer) head() *object.Commit {
	p.t.Helper()
	ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true)
	require.NoError(p.t, err)
	c, err := p.repo.CommitObject(ref.Hash())
	require.NoError(p.t, err)
	return c
}

func (p *peer) files(c *object.Commit) map[string]string {
	p.t.Helper()
	out := map[string]string{}
	it, err := c.Files()
	require.NoError(p.t, err)
	require.NoError(p.t, it.ForEach(func(f *object.File) error {
		s, err := f.Contents()
		out[f.Name] = s
		return err
	}))
	return out
}

// commit makes a commit on parent with kv applied (with() semantics: "" deletes),
// authored as the peer agent, signed by signer when non-nil.
func (p *peer) commit(parent *object.Commit, signer ssh.Signer, kv ...string) *object.Commit {
	p.t.Helper()
	p.n++
	files := with(p.files(parent), kv...)
	st := p.repo.Storer
	when := parent.Committer.When.Add(time.Hour + time.Duration(p.n)*time.Second).UTC()
	email := "peer-abcd1234+learn@agents.knomit.io"
	c := &object.Commit{
		Author:       object.Signature{Name: "peer-abcd1234", Email: email, When: when},
		Committer:    object.Signature{Name: "peer-abcd1234", Email: "peer-abcd1234@agents.knomit.io", When: when},
		Message:      fmt.Sprintf("learn: %d", p.n),
		TreeHash:     repoOn(p.t, st).tree(files),
		ParentHashes: []plumbing.Hash{parent.Hash},
	}
	if signer != nil {
		c = signed(p.t, c, signer)
	}
	o := st.NewEncodedObject()
	require.NoError(p.t, c.Encode(o))
	h, err := st.SetEncodedObject(o)
	require.NoError(p.t, err)
	got, err := object.GetCommit(st, h)
	require.NoError(p.t, err)
	return got
}

// push sends local refspecs; progress nil means no sideband is requested.
func (p *peer) push(url string, progress io.Writer, specs ...string) error {
	p.t.Helper()
	_ = p.repo.DeleteRemote("dst")
	_, err := p.repo.CreateRemote(&gogitconfig.RemoteConfig{Name: "dst", URLs: []string{url}})
	require.NoError(p.t, err)
	var rs []gogitconfig.RefSpec
	for _, s := range specs {
		rs = append(rs, gogitconfig.RefSpec(s))
	}
	return p.repo.Push(&gogit.PushOptions{RemoteName: "dst", RefSpecs: rs, Progress: progress})
}

// setBranch points the peer's local branch at c.
func (p *peer) setBranch(name string, c *object.Commit) {
	p.t.Helper()
	require.NoError(p.t, p.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), c.Hash)))
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	return &buf
}

func factAt(t *testing.T, svc *Service, branch, path string) (string, error) {
	t.Helper()
	res, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	return res.Content, err
}

// T1: an enrolled peer pushes its own branch, based on an OLDER host main.
// The branch is registered AND indexed on the host, and the first
// registration is not logged as a rewind (review N6).
func TestReceivePack_OwnBranchRegisteredAndIndexed(t *testing.T) {
	h := newPushHost(t, "", 2)
	p := newPeer(t, h.srv.URL)
	base := p.head()
	// Host main moves on after the peer cloned: the peer's branch is not a
	// descendant of the host's current main.
	_, err := h.svc.Facts().WriteFact(context.Background(), "main", "kb/notes/later.md",
		testFactBody("later", 0.9, nil), "later", "")
	require.NoError(t, err)

	c := base
	for i := range 3 {
		c = p.commit(c, nil, fmt.Sprintf("kb/notes/peer-%d.md", i), testFactBody(fmt.Sprintf("peer %d", i), 0.9, nil))
	}
	p.setBranch("work", c)
	logs := captureLog(t)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))

	require.Equal(t, c.Hash, h.ref(peerBranch), "the host's ref is the pushed tip")
	var id int64
	require.NoError(t, h.svc.rh.db.QueryRow(`SELECT id FROM branches WHERE name = ?`, peerBranch).Scan(&id))
	for i := range 3 {
		body, err := factAt(t, h.svc, peerBranch, fmt.Sprintf("kb/notes/peer-%d.md", i))
		require.NoError(t, err)
		require.Contains(t, body, fmt.Sprintf("peer %d", i))
		var n int
		require.NoError(t, h.svc.rh.db.QueryRow(`SELECT COUNT(*) FROM branch_facts WHERE branch_id = ? AND path = ?`,
			id, fmt.Sprintf("kb/notes/peer-%d.md", i)).Scan(&n))
		require.Equal(t, 1, n, "branch_facts row for the pushed fact")
	}
	// The peer's branch does not carry the host's later fact.
	_, err = factAt(t, h.svc, peerBranch, "kb/notes/later.md")
	require.Error(t, err)

	out := logs.String()
	require.Contains(t, out, "receive-pack: registered")
	for _, bad := range []string{"force-updated", "rewind", "DISJOINT", `"level":"warn"`} {
		require.NotContains(t, out, bad, "a first registration must not read as a rewind")
	}
	rep, err := h.svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.True(t, rep.IsClean(), rep.String())
	require.Contains(t, rep.Branches, peerBranch, "the pushed branch is a maintained, indexed branch")
}

// T2: every single-ref push outside the own-branch rule is refused with the
// rule's text, and nothing it sent is kept.
func TestReceivePack_RefusesOutsideOwnBranch(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	mainBefore, hostBefore, objsBefore := h.ref("main"), h.ref(hostBranch), h.objects()

	for _, tc := range []struct{ dst, want string }{
		{"refs/heads/main", "only your own agent branch may be pushed; refs/heads/main is not an agent branch"},
		{"refs/heads/agent/other-99999999", "refs/heads/agent/other-99999999 is not your agent branch (your certificate's fingerprint is abcd1234)"},
		{"refs/heads/foo", "only your own agent branch may be pushed; refs/heads/foo is not an agent branch"},
		{"refs/heads/exp/x", "refs/heads/exp/x is not an agent branch"},
		{"refs/heads/" + hostBranch, "is this host's own branch; it is written only by this host"},
	} {
		t.Run(tc.dst, func(t *testing.T) {
			err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:"+tc.dst)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
			require.Equal(t, objsBefore, h.objects(), "no object of a refused push is kept")
			require.Equal(t, mainBefore, h.ref("main"))
			require.Equal(t, hostBefore, h.ref(hostBranch))
			if tc.dst != "refs/heads/main" && tc.dst != "refs/heads/"+hostBranch {
				require.Equal(t, plumbing.ZeroHash, h.ref(strings.TrimPrefix(tc.dst, "refs/heads/")))
			}
		})
	}
}

// T2b: a push naming the OWN branch and a foreign one is refused WHOLE: the
// own branch does not move (review M3).
func TestReceivePack_TwoRefsRefusedWhole(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	objs := h.objects()

	err := p.push(h.srv.URL, &bytes.Buffer{},
		"+refs/heads/work:refs/heads/"+peerBranch, "+refs/heads/work:refs/heads/foo")
	require.Error(t, err)
	require.Contains(t, err.Error(), "a push may update exactly one ref")
	require.Equal(t, plumbing.ZeroHash, h.ref(peerBranch), "the own branch must NOT be registered")
	require.Equal(t, plumbing.ZeroHash, h.ref("foo"))
	require.Equal(t, objs, h.objects())
}

// rawPush sends a hand-built receive-pack request, for shapes a real client
// will not send (a delete without delete-refs, a stale old hash).
func rawPush(t *testing.T, url string, cmds ...*packp.Command) string {
	t.Helper()
	req := packp.NewReferenceUpdateRequest()
	require.NoError(t, req.Capabilities.Set(capability.ReportStatus))
	req.Commands = cmds
	var buf bytes.Buffer
	require.NoError(t, req.Encode(&buf))
	resp, err := http.Post(url+"/git-receive-pack", "application/x-git-receive-pack-request", &buf)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return fmt.Sprintf("%d %s", resp.StatusCode, b)
}

// T2 (delete): git never sends a delete without delete-refs, so a raw client.
func TestReceivePack_RefusesDelete(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))

	out := rawPush(t, h.srv.URL, &packp.Command{Name: plumbing.NewBranchReferenceName(peerBranch), Old: c.Hash, New: plumbing.ZeroHash})
	require.Contains(t, out, "ERR knomit: branch deletion is not allowed: refs/heads/"+peerBranch)
	require.Equal(t, c.Hash, h.ref(peerBranch))
}

// T15: the command's old hash is checked under the lock (git's contract).
func TestReceivePack_RefusesStaleOldHash(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	c1 := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	c2 := p.commit(c1, nil, "kb/notes/y.md", testFactBody("y", 0.9, nil))
	p.setBranch("work", c2)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))

	// c1 is on the host; claim the branch was at c1 (it is at c2).
	out := rawPush(t, h.srv.URL, &packp.Command{Name: plumbing.NewBranchReferenceName(peerBranch), Old: c1.Hash, New: c1.Hash})
	require.Contains(t, out, "moved since it was advertised")
	require.Equal(t, c2.Hash, h.ref(peerBranch))
}

// T3: under enforce at the HOST's main, a commit whose key has no member
// record refuses the push — even though the pushed branch itself turns
// verification off in its own ontology (review M4: the mode is read at the
// host's upstream, never at the pushed tip).
func TestReceivePack_EnforceRefusesUnacceptedSigner(t *testing.T) {
	h := newPushHost(t, VerifyEnforce, 1)
	h.setPolicy(PushPolicy{Pusher: pushFP, OwnBranch: hostBranch, Members: h.members([]FleetMember{}, nil)})
	p := newPeer(t, h.srv.URL)
	k := newTestSigner(t)
	c1 := p.commit(p.head(), k, fact.OntologyFile, kbOntology(VerifyOff))
	c2 := p.commit(c1, k, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c2)
	objs := h.objects()

	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refused by verify_signatures=enforce: "+RuleNoRecord)
	require.Contains(t, err.Error(), c2.Hash.String(), "the refusal names the commit")
	require.Equal(t, plumbing.ZeroHash, h.ref(peerBranch))
	require.Equal(t, objs, h.objects())
}

// T3 positive control: the same push, signed by the key the member record
// names, is accepted under enforce — the judge is really judging.
func TestReceivePack_EnforceAcceptsMemberKey(t *testing.T) {
	h := newPushHost(t, VerifyEnforce, 1)
	k := newTestSigner(t)
	m := FleetMember{Member: fact.Member{Agent: "peer-abcd1234", State: fact.MemberActive, Key: k.PublicKey()}}
	h.setPolicy(PushPolicy{Pusher: pushFP, OwnBranch: hostBranch, Members: h.members([]FleetMember{m}, nil)})
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), k, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))
	require.Equal(t, c.Hash, h.ref(peerBranch))
}

// T4: off never touches the fleet; log judges, logs and accepts.
func TestReceivePack_OffAndLogAccept(t *testing.T) {
	for _, mode := range []string{"", VerifyOff, VerifyLog} {
		t.Run("mode="+mode, func(t *testing.T) {
			h := newPushHost(t, mode, 1)
			h.setPolicy(PushPolicy{Pusher: pushFP, OwnBranch: hostBranch, Members: h.members([]FleetMember{}, nil)})
			p := newPeer(t, h.srv.URL)
			c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
			p.setBranch("work", c)
			logs := captureLog(t)
			require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))
			require.Equal(t, c.Hash, h.ref(peerBranch))
			if mode == VerifyLog {
				require.Equal(t, int32(1), h.membersCalls.Load())
				require.Contains(t, logs.String(), "would be refused (verify_signatures=log)")
				require.Contains(t, logs.String(), c.Hash.String())
			} else {
				require.Equal(t, int32(0), h.membersCalls.Load(), "off must never read the fleet")
			}
		})
	}
}

// T5: enforce with no fleet repository refuses (the gate could not run).
func TestReceivePack_EnforceWithoutFleetRefuses(t *testing.T) {
	h := newPushHost(t, VerifyEnforce, 1)
	h.setPolicy(PushPolicy{Pusher: pushFP, OwnBranch: hostBranch,
		Members: h.members(nil, fmt.Errorf("this instance is standalone"))})
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), newTestSigner(t), "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no fleet repository to judge against")
	require.Equal(t, plumbing.ZeroHash, h.ref(peerBranch))
}

// T6: a non-fast-forward push is accepted and the index follows it: the
// dropped fact is gone at the branch, the new one is there, and Verify finds
// no stale branch_commits row for the dropped commit.
func TestReceivePack_NonFastForwardIndexFollows(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	base := p.head()
	a1 := p.commit(base, nil, "kb/notes/x.md", testFactBody("x", 0.9, nil), "kb/notes/y.md", testFactBody("y", 0.9, nil))
	p.setBranch("work", a1)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))

	a2 := p.commit(base, nil, "kb/notes/x.md", testFactBody("x", 0.9, nil), "kb/notes/z.md", testFactBody("z", 0.9, nil))
	p.setBranch("work", a2)
	logs := captureLog(t)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))
	require.Equal(t, a2.Hash, h.ref(peerBranch))
	require.Contains(t, logs.String(), "receive-pack: force-updated")

	_, err := factAt(t, h.svc, peerBranch, "kb/notes/y.md")
	require.Error(t, err, "y was dropped by the rewrite")
	body, err := factAt(t, h.svc, peerBranch, "kb/notes/z.md")
	require.NoError(t, err)
	require.Contains(t, body, "z")
	var n int
	require.NoError(t, h.svc.rh.db.QueryRow(`SELECT COUNT(*) FROM branch_commits
		WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND commit_hash = ?`, peerBranch, a1.Hash.String()).Scan(&n))
	require.Equal(t, 0, n, "the dropped commit is no longer on the branch")
	rep, err := h.svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.True(t, rep.IsClean(), rep.String())
}

// T7: the host's own branch is untouched by a peer push and still writable.
func TestReceivePack_HostOwnBranchUnaffected(t *testing.T) {
	h := newPushHost(t, "", 1)
	before := h.ref(hostBranch)
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))
	require.Equal(t, before, h.ref(hostBranch))
	_, err := h.svc.Facts().WriteFact(context.Background(), hostBranch, "kb/notes/mine.md", testFactBody("mine", 0.9, nil), "m", "")
	require.NoError(t, err)
	require.NotEqual(t, before, h.ref(hostBranch))
	require.Equal(t, c.Hash, h.ref(peerBranch))
}

// T8: the receive-pack advertisement is the curated view without HEAD, and
// offers exactly the four capabilities — never delete-refs.
func TestReceivePack_Advertisement(t *testing.T) {
	h := newPushHost(t, "", 1)
	require.NoError(t, h.svc.rh.gits.SetReference(plumbing.NewHashReference("refs/remotes/origin/secret", h.ref("main"))))
	resp, err := http.Get(h.srv.URL + "/info/refs?service=git-receive-pack")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "application/x-git-receive-pack-advertisement", resp.Header.Get("Content-Type"))
	ar := packp.NewAdvRefs()
	require.NoError(t, ar.Decode(resp.Body))
	require.Nil(t, ar.Head, "receive-pack advertises no HEAD")
	names := []string{}
	for n := range ar.References {
		names = append(names, n)
	}
	require.ElementsMatch(t, []string{"refs/heads/main", "refs/heads/" + hostBranch}, names)
	var caps []string
	for _, c := range ar.Capabilities.All() {
		caps = append(caps, string(c))
	}
	require.ElementsMatch(t, []string{"agent", "report-status", "side-band-64k", "ofs-delta"}, caps)
	require.False(t, ar.Capabilities.Supports(capability.DeleteRefs))
}

// T8 (policy): no policy, or a whole-request refusal, answers the
// advertisement with an ERR a client renders.
func TestReceivePack_AdvertisementRefusals(t *testing.T) {
	h := newPushHost(t, "", 1)
	h.setPolicy(PushPolicy{Refusal: "knomit: nope for a reason"})
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)
	require.Error(t, err)
	require.Contains(t, err.Error(), "knomit: nope for a reason")

	// Straight at the store handler with no policy at all.
	srv := httptest.NewServer(h.svc.Handler())
	t.Cleanup(srv.Close)
	err = p.push(srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)
	require.Error(t, err)
	require.Contains(t, err.Error(), errNoPushPolicy)
	out := rawPush(t, srv.URL, &packp.Command{Name: plumbing.NewBranchReferenceName(peerBranch), New: c.Hash})
	require.Contains(t, out, errNoPushPolicy)
	require.Equal(t, plumbing.ZeroHash, h.ref(peerBranch))
}

// T9: the host's own origin push carries its own branch only, never a
// peer's registered branch.
func TestReceivePack_HostPushDoesNotForwardPeerBranch(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))

	bare := t.TempDir()
	_, err := gogit.PlainInit(bare, true)
	require.NoError(t, err)
	require.NoError(t, h.svc.ConfigureRemote(bare, "main", hostBranch))
	_, err = h.svc.Remote().Push(context.Background(), hostBranch, nil)
	require.NoError(t, err)
	origin, err := gogit.PlainOpen(bare)
	require.NoError(t, err)
	refs, err := origin.References()
	require.NoError(t, err)
	var got []string
	require.NoError(t, refs.ForEach(func(r *plumbing.Reference) error { got = append(got, r.Name().String()); return nil }))
	require.Contains(t, got, "refs/heads/"+hostBranch)
	require.NotContains(t, got, "refs/heads/"+peerBranch)
}

// T13 (pin, no sabotage): real git renders an accepted push, a forced push
// and a refusal naming the rule.
func TestReceivePack_RealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h := newPushHost(t, "", 1)
	dir := t.TempDir()
	gitOut(t, dir, "clone", h.srv.URL, "c")
	c := filepath.Join(dir, "c")
	gitOut(t, c, "config", "user.email", "x@y")
	gitOut(t, c, "config", "user.name", "x")
	require.NoError(t, os.WriteFile(filepath.Join(c, "kb", "notes", "real.md"), []byte(testFactBody("real", 0.9, nil)), 0o644))
	gitOut(t, c, "add", ".")
	gitOut(t, c, "commit", "-m", "real")

	gitOut(t, c, "push", h.srv.URL, "HEAD:refs/heads/"+peerBranch)
	gitOut(t, c, "commit", "--amend", "-m", "real2")
	gitOut(t, c, "push", "--force", h.srv.URL, "HEAD:refs/heads/"+peerBranch)
	require.Equal(t, strings.TrimSpace(gitOut(t, c, "rev-parse", "HEAD")), h.ref(peerBranch).String())

	cmd := exec.Command("git", "push", h.srv.URL, "HEAD:refs/heads/main")
	cmd.Dir = c
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "remote error: knomit: only your own agent branch may be pushed")
}

// T14: a client that asked for no sideband still reads the refusal.
func TestReceivePack_RefusalWithoutSideband(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	err := p.push(h.srv.URL, nil, "+refs/heads/work:refs/heads/main")
	require.Error(t, err)
	require.Contains(t, err.Error(), "only your own agent branch may be pushed")
}

// T16: a client that goes away once the push is accepted cannot abort the
// registration: the ref, the branches row, branch_commits and branch_facts
// all describe the pushed tip (review M1).
func TestReceivePack_CancelAfterAcceptStillRegistersWhole(t *testing.T) {
	h := newPushHost(t, "", 1)
	p := newPeer(t, h.srv.URL)
	c := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", c)
	h.mu.Lock()
	h.cancelOnRegister = true
	h.mu.Unlock()
	_ = p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)

	require.Equal(t, c.Hash, h.ref(peerBranch))
	var n int
	require.NoError(t, h.svc.rh.db.QueryRow(`SELECT COUNT(*) FROM branch_commits
		WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND commit_hash = ?`, peerBranch, c.Hash.String()).Scan(&n))
	require.Equal(t, 1, n, "branch_commits describes the tip")
	require.NoError(t, h.svc.rh.db.QueryRow(`SELECT COUNT(*) FROM branch_facts
		WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND path = ?`, peerBranch, "kb/notes/x.md").Scan(&n))
	require.Equal(t, 1, n, "branch_facts describes the tip")
	rep, err := h.svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.True(t, rep.IsClean(), rep.String())
}

// N9: the new-commit walk stops at the host's commits — it visits exactly the
// pushed commits, however long the host's history is.
func TestReceivePack_NewCommitWalkIsProportionalToThePush(t *testing.T) {
	h := newPushHost(t, "", 30)
	p := newPeer(t, h.srv.URL)
	c1 := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	c2 := p.commit(c1, nil, "kb/notes/y.md", testFactBody("y", 0.9, nil))

	q := newQuarantine(h.svc.rh.gits)
	for _, c := range []*object.Commit{c1, c2} {
		for _, h := range []plumbing.Hash{c.Hash, c.TreeHash} {
			o, err := p.repo.Storer.EncodedObject(plumbing.AnyObject, h)
			require.NoError(t, err)
			_, err = q.SetEncodedObject(o)
			require.NoError(t, err)
		}
	}
	commits, visited, err := q.newCommits(c2.Hash)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	require.Equal(t, c2.Hash, commits[0].Hash)
	require.Equal(t, 2, visited, "the walk must not descend into the host's %d-commit history", 31)
}
