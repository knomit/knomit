package repos

// F08 PR B — `consensus: off|auto` (T-B1..T-B9). A host instance serves its
// repo over an in-process /git (the store's own handler, behind the push
// policy the web edge would attach for an enrolled peer), and a peer instance
// clones it, writes, and runs its ordinary sync (Sync, then Push) by hand.
//
// Nothing here spells the consensus branch: it is asked of the store
// (UpstreamBranch). On a host with no origin the store names it "main" by
// construction; T-B3 runs on a host whose consensus branch is "develop".

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

const (
	cHostAgent = "agent/host-11111111"
	cPeerAgent = "agent/peer-abcd1234"
	// cPeerFP is the pushing peer's full key fingerprint; its first 8 hex
	// name the only branch it may push (cPeerAgent).
	cPeerFP = "abcd1234" + "00000000000000000000000000000000000000000000000000000000"
)

const autoAttrs = "attributes:\n  consensus: auto\n"

type cHost struct {
	m   *Manager
	ri  *RepoInstance
	url string
}

type hostOpts struct {
	background bool // run the local reconcile loop (interval 1 min: only a wake moves main fast)
	readOnly   bool
}

// newConsensusHost boots the host repo "kb" with ontology ont on its agent
// branch AND its consensus branch, and serves it.
func newConsensusHost(t *testing.T, ont string, o hostOpts) *cHost {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Home: home, OntologyRoot: "kb", ReadOnly: o.readOnly}
	cfg.Git.LocalReconcileInterval = time.Minute
	m := New(context.Background(), Deps{
		Cfg:                   cfg,
		AgentBranch:           cHostAgent,
		KeyPath:               filepath.Join(home, "agent.key"),
		Signer:                testsigner.Named("consensus-host"),
		DisableBackgroundSync: !o.background,
	})
	t.Cleanup(func() { m.Close() })
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "kb")
	h := &cHost{m: m, ri: ri}
	if ont != "" {
		h.setOntology(t, ont)
	}
	writeOn(t, ri, cHostAgent, "kb/tasks/first.md")
	h.advance(t)
	h.url = serveConsensusGit(t, ri)
	return h
}

// serveConsensusGit serves ri's git endpoints with the push policy of an
// enrolled peer whose fingerprint is cPeerFP.
func serveConsensusGit(t *testing.T, ri *RepoInstance) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc, release, err := ri.Acquire()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer release()
		ctx := store.WithPushPolicy(r.Context(), store.PushPolicy{Pusher: cPeerFP, OwnBranch: ri.AgentBranch()})
		svc.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (h *cHost) svc(t *testing.T) *store.Service { return testService(t, h.ri) }

func (h *cHost) upstream(t *testing.T) string { return h.svc(t).UpstreamBranch() }

// setOntology commits ont on the agent branch and moves the consensus
// branch up to it (the host's local reconcile, run by hand).
func (h *cHost) setOntology(t *testing.T, ont string) {
	t.Helper()
	_, err := h.svc(t).Facts().WriteFact(context.Background(), cHostAgent, OntologyPath, ont, "ontology", "updated")
	require.NoError(t, err)
	h.advance(t)
}

// advance is one round of the host's local reconcile.
func (h *cHost) advance(t *testing.T) {
	t.Helper()
	svc := h.svc(t)
	_, err := svc.AdvanceLocalUpstream(context.Background(), cHostAgent, svc.UpstreamBranch())
	require.NoError(t, err)
}

func (h *cHost) tip(t *testing.T, branch string) string {
	t.Helper()
	c, err := h.svc(t).Branches().HeadCommit(context.Background(), branch)
	if errors.Is(err, store.ErrBranchNotFound) {
		return ""
	}
	require.NoError(t, err)
	return c
}

func (h *cHost) has(t *testing.T, branch, path string) bool {
	t.Helper()
	_, err := h.svc(t).Facts().ReadFact(context.Background(), branch, path, nil)
	return err == nil
}

func (h *cHost) content(t *testing.T, branch, path string) string {
	t.Helper()
	f, err := h.svc(t).Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	return f.Content
}

func (h *cHost) stats() consensusStats { return h.ri.consensus.snapshot() }

// settle waits for a merger run that STARTED after this call: kick, wait a
// run; kick, wait another. Runs are sequential, so the second began after
// the first finished, which was after this call began.
func (h *cHost) settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 2; i++ {
		before := h.stats().Runs
		h.ri.consensus.kickNow()
		require.Eventually(t, func() bool { return h.stats().Runs > before }, 10*time.Second, 5*time.Millisecond,
			"the consensus merger never ran")
	}
}

// commits is every commit reachable from any branch the host serves, seen
// through a fresh clone: the whole history two instances have produced.
func (h *cHost) commits(t *testing.T) map[plumbing.Hash]bool {
	t.Helper()
	repo, err := gogit.Clone(memory.NewStorage(), nil, &gogit.CloneOptions{URL: h.url})
	require.NoError(t, err)
	refs, err := repo.References()
	require.NoError(t, err)
	out := map[plumbing.Hash]bool{}
	require.NoError(t, refs.ForEach(func(r *plumbing.Reference) error {
		if r.Type() != plumbing.HashReference {
			return nil
		}
		it, err := repo.Log(&gogit.LogOptions{From: r.Hash()})
		if err != nil {
			return err
		}
		return it.ForEach(func(c *object.Commit) error { out[c.Hash] = true; return nil })
	}))
	return out
}

type cPeer struct {
	m  *Manager
	ri *RepoInstance
}

// newConsensusPeer clones the host's repo as instance cPeerAgent, signing
// with its own key (so the host tells its commits from its own).
func newConsensusPeer(t *testing.T, url string) *cPeer {
	t.Helper()
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch:           cPeerAgent,
		KeyPath:               filepath.Join(home, "agent.key"),
		Signer:                testsigner.Named("consensus-peer"),
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { m.Close() })
	require.NoError(t, m.Start())
	ri, err := m.Create(context.Background(), CreateSpec{Name: "kb", Mode: "clone", Origin: &OriginSpec{URL: url}}, nil)
	require.NoError(t, err)
	return &cPeer{m: m, ri: ri}
}

func (p *cPeer) write(t *testing.T, path, title string) string {
	t.Helper()
	r, err := testService(t, p.ri).Facts().WriteFact(context.Background(), cPeerAgent, path, factBody(title), "learn: "+path, "learn")
	require.NoError(t, err)
	return r.CommitHash
}

// round is the peer's ordinary sync: fetch and reconcile, then push its own
// branch. It returns once the push POST has returned.
func (p *cPeer) round(t *testing.T) {
	t.Helper()
	svc := testService(t, p.ri)
	_, err := svc.Remote().Sync(context.Background(), cPeerAgent, nil)
	require.NoError(t, err)
	_, err = svc.Remote().Push(context.Background(), cPeerAgent, nil)
	require.NoError(t, err)
}

func (p *cPeer) tip(t *testing.T) string {
	t.Helper()
	c, err := testService(t, p.ri).Branches().HeadCommit(context.Background(), cPeerAgent)
	require.NoError(t, err)
	return c
}

func setConsensusHooks(t *testing.T, h consensusHooks) {
	t.Helper()
	consensusHooksMu.Lock()
	consensusTestHooks = h
	consensusHooksMu.Unlock()
	t.Cleanup(func() {
		consensusHooksMu.Lock()
		consensusTestHooks = consensusHooks{}
		consensusHooksMu.Unlock()
	})
}

func warningsWith(s consensusStats, needle string) int {
	n := 0
	for _, w := range s.Warnings {
		if strings.Contains(w, needle) {
			n++
		}
	}
	return n
}

// T-B1: with `consensus: auto` at the host's consensus branch, a peer's push
// is merged into the host's AGENT branch, under a merge commit [agent tip,
// peer tip] the host signed, and the consensus branch follows through the
// wake (the loop's interval is a minute) within 3 s. No kick by the test:
// the push itself must start it.
//
// SABOTAGE: MergeConsensus's target switched from m.agentBranch to the
// upstream → the agent branch never gets the peer's fact → red.
func TestConsensus_AutoMergesPushIntoAgentBranch(t *testing.T) {
	h := newConsensusHost(t, triggerOntology(autoAttrs), hostOpts{background: true})
	up := h.upstream(t)
	require.Equal(t, h.tip(t, cHostAgent), h.tip(t, up), "fixture: the consensus branch starts at the agent tip")
	agentBefore := h.tip(t, cHostAgent)

	p := newConsensusPeer(t, h.url)
	p.write(t, "kb/tasks/peer.md", "peer")
	p.round(t)
	peerTip := p.tip(t)

	require.Eventually(t, func() bool { return h.has(t, cHostAgent, "kb/tasks/peer.md") }, 2*time.Second, 10*time.Millisecond,
		"the peer's fact must reach the host's AGENT branch")
	svc := h.svc(t)
	tip, err := svc.Branches().HeadCommit(context.Background(), cHostAgent)
	require.NoError(t, err)
	commits := h.commitsOf(t)
	mc := commits[plumbing.NewHash(tip)]
	require.NotNil(t, mc)
	require.Equal(t, []plumbing.Hash{plumbing.NewHash(agentBefore), plumbing.NewHash(peerTip)}, mc.ParentHashes,
		"one merge commit [host agent tip, peer tip]")
	require.NotEmpty(t, mc.PGPSignature, "the merge commit is signed")
	require.Contains(t, mc.Author.Email, "host-11111111", "authored by the host")
	require.Contains(t, mc.Message, "into "+cHostAgent)

	require.Eventually(t, func() bool { return h.tip(t, up) == tip }, 3*time.Second, 10*time.Millisecond,
		"the consensus branch follows the agent branch through the wake")
	require.Equal(t, 1, h.stats().Merges)
}

// commitsOf reads the commits of a clone of the host, by hash.
func (h *cHost) commitsOf(t *testing.T) map[plumbing.Hash]*object.Commit {
	t.Helper()
	repo, err := gogit.Clone(memory.NewStorage(), nil, &gogit.CloneOptions{URL: h.url})
	require.NoError(t, err)
	out := map[plumbing.Hash]*object.Commit{}
	it, err := repo.CommitObjects()
	require.NoError(t, err)
	require.NoError(t, it.ForEach(func(c *object.Commit) error { out[c.Hash] = c; return nil }))
	return out
}

// T-B2: absent, explicit off and an unknown value never merge; the unknown
// value is warned exactly once however many runs read it.
//
// SABOTAGE: reading an invalid value as on (`return cs.Mode ==
// fact.ConsensusAuto || !cs.Valid` in autoAt) → "bogus" merges → red.
func TestConsensus_OffDoesNothing(t *testing.T) {
	for _, c := range []struct{ name, attrs string }{
		{"absent", ""},
		{"off", "attributes:\n  consensus: off\n"},
		{"bogus", "attributes:\n  consensus: recipe:merge\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newConsensusHost(t, triggerOntology(c.attrs), hostOpts{})
			agentBefore := h.tip(t, cHostAgent)
			p := newConsensusPeer(t, h.url)
			p.write(t, "kb/tasks/peer.md", "peer")
			p.round(t)
			h.settle(t)
			h.settle(t)
			require.Equal(t, agentBefore, h.tip(t, cHostAgent), "nothing merged")
			require.False(t, h.has(t, cHostAgent, "kb/tasks/peer.md"))
			s := h.stats()
			require.Zero(t, s.Merges)
			if c.name == "bogus" {
				require.Equal(t, 1, warningsWith(s, `unknown value recipe:merge`), "warned once: %v", s.Warnings)
			} else {
				require.Empty(t, s.Warnings)
			}
		})
	}
}

// T-B3: a host WITH an origin ignores `consensus: auto` — the origin owns
// its consensus branch — and says so exactly once, naming the origin. This
// host's consensus branch is "develop": the setting is read at develop's
// tip, where it is auto, and at no other branch.
//
// SABOTAGE: dropping the origin check → the push is merged → red. (A reader
// that looked at a hardcoded "main" would find no auto and never warn → red.)
func TestConsensus_OriginRepoUnaffected(t *testing.T) {
	h := newConsensusHost(t, triggerOntology(""), hostOpts{})
	svc := h.svc(t)
	const originURL = "https://example.invalid/consensus/kb.git"
	svc.SetOrigin(&store.Origin{URL: originURL, Branch: "develop"})
	up := h.upstream(t)
	require.Equal(t, "develop", up)
	require.NotEqual(t, "main", up, "this test's consensus branch is not main")
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), up, cHostAgent))
	_, err := svc.Facts().WriteFact(context.Background(), up, OntologyPath, triggerOntology(autoAttrs), "ontology", "updated")
	require.NoError(t, err)

	agentBefore := h.tip(t, cHostAgent)
	p := newConsensusPeer(t, h.url)
	p.write(t, "kb/tasks/peer.md", "peer")
	p.round(t)
	h.settle(t)
	h.settle(t)

	require.Equal(t, agentBefore, h.tip(t, cHostAgent), "nothing merged on a repo with an origin")
	s := h.stats()
	require.Zero(t, s.Merges)
	require.Equal(t, 1, warningsWith(s, originURL), "exactly one warning naming the origin: %v", s.Warnings)
	require.Equal(t, 1, warningsWith(s, "the origin owns develop"), "and the branch it owns: %v", s.Warnings)
}

// T-B4a: two idle instances converge. The peer writes once and its push is
// merged; the host writes once. Then K full rounds with no writes — the
// peer's sync and push, the merger (forced to run), the host's local
// reconcile — add ZERO commits anywhere, and the merger tried the pushed
// branch in every round (so "zero commits" is not "never ran").
//
// SABOTAGE (reviewer X7b): in branch_merge.go, `if isSrcAncestor {` →
// `if isSrcAncestor && !o.record {` → round 1 records the peer's
// already-merged tip again → red.
func TestConsensus_Quiescent(t *testing.T) {
	const K = 5
	h := newConsensusHost(t, triggerOntology(autoAttrs), hostOpts{})
	p := newConsensusPeer(t, h.url)

	p.write(t, "kb/tasks/peer.md", "peer")
	p.round(t)
	h.settle(t)
	require.True(t, h.has(t, cHostAgent, "kb/tasks/peer.md"), "the first exchange merged")
	writeOn(t, h.ri, cHostAgent, "kb/tasks/host.md") // the host writes once, after the merge

	before := h.commits(t)
	attempts := h.stats().Attempts
	for i := 0; i < K; i++ {
		p.round(t)
		h.settle(t)
		h.advance(t)
	}
	after := h.commits(t)
	for c := range after {
		require.True(t, before[c], "round wrote a new commit %s", c)
	}
	require.Len(t, after, len(before), "zero new commits in %d idle rounds", K)
	require.True(t, before[plumbing.NewHash(p.tip(t))], "the peer's branch only fast-forwarded")
	require.Equal(t, h.tip(t, cHostAgent), h.tip(t, h.upstream(t)), "converged: the consensus branch is the agent tip")
	require.Equal(t, h.tip(t, cHostAgent), p.tip(t), "converged: the peer is at the host's tip")
	require.GreaterOrEqual(t, h.stats().Attempts-attempts, K, "the merger tried the pushed branch every round")
}

// T-B4b: the peer writes AGAIN before it has synced the host's merge, so its
// own sync writes a real merge commit (case b: a peer whose sync creates
// merge commits). Once both are idle, rounds add zero commits.
//
// SABOTAGE: X7b as in T-B4a → red.
func TestConsensus_QuiescentAfterPeerMergeCommit(t *testing.T) {
	const K = 5
	h := newConsensusHost(t, triggerOntology(autoAttrs), hostOpts{})
	p := newConsensusPeer(t, h.url)

	p.write(t, "kb/tasks/p1.md", "p1")
	p.round(t)
	h.settle(t)
	writeOn(t, h.ri, cHostAgent, "kb/tasks/host.md") // the host writes once
	h.advance(t)
	p.write(t, "kb/tasks/p2.md", "p2") // before syncing the host's merge of p1
	p.round(t)                         // its sync merges the consensus branch (with host.md) into p2: a merge commit
	peerMerge := commitIn(t, h.commitsOf(t), plumbing.NewHash(p.tip(t)))
	require.Len(t, peerMerge.ParentHashes, 2, "fixture: the peer's sync wrote a merge commit")
	require.Contains(t, peerMerge.Author.Email, "peer-abcd1234")
	h.settle(t)
	require.True(t, h.has(t, cHostAgent, "kb/tasks/p2.md"))

	before := h.commits(t)
	attempts := h.stats().Attempts
	for i := 0; i < K; i++ {
		p.round(t)
		h.settle(t)
		h.advance(t)
	}
	after := h.commits(t)
	for c := range after {
		require.True(t, before[c], "an idle round wrote a new commit %s", c)
	}
	require.Equal(t, h.tip(t, cHostAgent), p.tip(t), "converged")
	require.GreaterOrEqual(t, h.stats().Attempts-attempts, K)
}

func commitIn(t *testing.T, all map[plumbing.Hash]*object.Commit, h plumbing.Hash) *object.Commit {
	t.Helper()
	c := all[h]
	require.NotNil(t, c, "commit %s not served by the host", h)
	return c
}

// noffPeer is a peer that is not a knomit instance and whose "sync" is
// `git merge --no-ff` of the consensus branch: whenever the consensus
// branch is not already in its branch it writes a merge commit, even when a
// fast-forward was possible.
type noffPeer struct {
	repo     *gogit.Repository
	upstream string
	tip      plumbing.Hash
}

func newNoffPeer(t *testing.T, url, upstream string) *noffPeer {
	t.Helper()
	repo, err := gogit.Clone(memory.NewStorage(), nil, &gogit.CloneOptions{URL: url})
	require.NoError(t, err)
	return &noffPeer{repo: repo, upstream: upstream}
}

func (n *noffPeer) upstreamTip(t *testing.T) *object.Commit {
	t.Helper()
	err := n.repo.Fetch(&gogit.FetchOptions{RemoteName: "origin", Force: true, RefSpecs: []gogitconfig.RefSpec{
		gogitconfig.RefSpec("+refs/heads/" + n.upstream + ":refs/remotes/origin/" + n.upstream)}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		require.NoError(t, err)
	}
	ref, err := n.repo.Reference(plumbing.NewRemoteReferenceName("origin", n.upstream), true)
	require.NoError(t, err)
	c, err := n.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	return c
}

func (n *noffPeer) commit(t *testing.T, c *object.Commit) plumbing.Hash {
	t.Helper()
	st := n.repo.Storer
	o := st.NewEncodedObject()
	require.NoError(t, c.Encode(o))
	h, err := st.SetEncodedObject(o)
	require.NoError(t, err)
	n.tip = h
	return h
}

func noffSig() object.Signature {
	return object.Signature{Name: "peer-abcd1234", Email: "peer-abcd1234@agents.knomit.io", When: time.Unix(1790000000, 0).UTC()}
}

// author writes one authored commit on top of the consensus branch.
func (n *noffPeer) author(t *testing.T) {
	t.Helper()
	parent := n.upstreamTip(t)
	st := n.repo.Storer
	blob := st.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, _ := blob.Writer()
	_, _ = w.Write([]byte("noff peer\n"))
	_ = w.Close()
	bh, err := st.SetEncodedObject(blob)
	require.NoError(t, err)
	ptree, err := parent.Tree()
	require.NoError(t, err)
	entries := append([]object.TreeEntry{}, ptree.Entries...)
	entries = append(entries, object.TreeEntry{Name: "noff.md", Mode: filemode.Regular, Hash: bh})
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Name < entries[j-1].Name; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	to := st.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: entries}).Encode(to))
	th, err := st.SetEncodedObject(to)
	require.NoError(t, err)
	n.commit(t, &object.Commit{Author: noffSig(), Committer: noffSig(), Message: "noff: author",
		TreeHash: th, ParentHashes: []plumbing.Hash{parent.Hash}})
}

// sync is `git fetch && git merge --no-ff origin/<upstream>`: a merge commit
// whenever the consensus branch is not already in the peer's branch. The
// host never edits the peer's files, so the merge result is the consensus
// branch's tree.
func (n *noffPeer) sync(t *testing.T) {
	t.Helper()
	up := n.upstreamTip(t)
	mine, err := n.repo.CommitObject(n.tip)
	require.NoError(t, err)
	if in, err := up.IsAncestor(mine); err == nil && in || up.Hash == mine.Hash {
		return // already up to date
	}
	n.commit(t, &object.Commit{Author: noffSig(), Committer: noffSig(), Message: "merge origin/" + n.upstream,
		TreeHash: up.TreeHash, ParentHashes: []plumbing.Hash{mine.Hash, up.Hash}})
}

func (n *noffPeer) push(t *testing.T) {
	t.Helper()
	require.NoError(t, n.repo.Storer.SetReference(plumbing.NewHashReference("refs/heads/work", n.tip)))
	err := n.repo.Push(&gogit.PushOptions{RemoteName: "origin", Progress: &bytes.Buffer{},
		RefSpecs: []gogitconfig.RefSpec{gogitconfig.RefSpec("+refs/heads/work:refs/heads/" + cPeerAgent)}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		require.NoError(t, err)
	}
}

// T-B4c: a peer whose sync ALWAYS writes a merge commit (never
// fast-forwards). Without a rule for it, every host merge commit is merged
// back by the peer with a merge commit of its own, which the host merges
// again: one commit per round, forever. MergeConsensus no-ops a tip whose
// new commits are all merges and change nothing, so after the first exchange
// the host writes NOTHING, while the merger keeps trying every round.
//
// SABOTAGE: dropping `skipMergeOnly: true` from MergeConsensus → a host
// merge commit every round → red.
func TestConsensus_QuiescentWithNoFFPeer(t *testing.T) {
	const K = 5
	h := newConsensusHost(t, triggerOntology(autoAttrs), hostOpts{})
	n := newNoffPeer(t, h.url, h.upstream(t))

	n.author(t)
	n.push(t)
	h.settle(t)
	h.advance(t)
	merged := h.tip(t, cHostAgent)
	require.Equal(t, 1, h.stats().Merges, "the peer's authored commit is merged")

	attempts := h.stats().Attempts
	for i := 0; i < K; i++ {
		n.sync(t)
		n.push(t)
		h.settle(t)
		h.advance(t)
		require.Equal(t, merged, h.tip(t, cHostAgent), "round %d: the host wrote a commit for a merge-only, change-free peer tip", i+1)
	}
	require.Equal(t, 1, h.stats().Merges)
	require.GreaterOrEqual(t, h.stats().Attempts-attempts, K, "the merger tried the pushed branch every round")
}

// T-B5: a conflict is never resolved by the merger. The same fact changed on
// both sides → nothing merged, the host's ref unmoved, ONE warning naming the
// path, and the branch still listed as pushed with commits to merge. Re-runs
// on the SAME peer tip do not retry. Then the peer's own sync merges the
// host's consensus branch with LocalWins (its version kept) and pushes: the
// merge base is now the host's commit, so the retry merges CLEANLY and the
// PEER's version lands (review F1 — pinned as it is; the user rules on it).
//
// SABOTAGE: StrategyLocalWins in MergeConsensus → the first merge goes
// through with the host's version kept silently → "nothing merged" red.
func TestConsensus_ConflictLeftForHuman(t *testing.T) {
	h := newConsensusHost(t, triggerOntology(autoAttrs), hostOpts{})
	writeOn(t, h.ri, cHostAgent, "kb/tasks/shared.md")
	h.advance(t)
	p := newConsensusPeer(t, h.url)

	_, err := testService(t, p.ri).Facts().WriteFact(context.Background(), cPeerAgent, "kb/tasks/shared.md", factBody("peer's version"), "edit", "update")
	require.NoError(t, err)
	_, err = h.svc(t).Facts().WriteFact(context.Background(), cHostAgent, "kb/tasks/shared.md", factBody("host's version"), "edit", "update")
	require.NoError(t, err)
	agentBefore := h.tip(t, cHostAgent)

	p.round(t) // the consensus branch has not moved: the peer pushes its edit as is
	h.settle(t)
	s := h.stats()
	require.Equal(t, agentBefore, h.tip(t, cHostAgent), "a conflict moves nothing")
	require.Equal(t, 1, s.Refused)
	require.Zero(t, s.Merges)
	require.Equal(t, 1, warningsWith(s, "conflicting paths kb/tasks/shared.md"), "one warning: %v", s.Warnings)
	require.True(t, h.ri.IsPushedBranch(cPeerAgent))
	info, err := h.svc(t).PushedBranches(context.Background(), []string{cPeerAgent}, cHostAgent, "kb")
	require.NoError(t, err)
	require.Len(t, info, 1)
	require.Positive(t, info[0].ToMerge, "left in the Pushed branches list for a human")

	attempts := s.Attempts
	h.settle(t)
	h.settle(t)
	s = h.stats()
	require.Equal(t, attempts, s.Attempts, "the same refused tip is not retried")
	require.Equal(t, 1, s.Refused)
	require.Equal(t, 1, warningsWith(s, "conflicting paths"), "still one warning: %v", s.Warnings)

	// F1: the peer's sync takes the host's consensus branch with LocalWins
	// and pushes the result; the retry is clean and the peer's version wins.
	// (The host's branch carries one more fact, so the peer's merge is a real
	// merge commit rather than a tree-identical no-op.)
	writeOn(t, h.ri, cHostAgent, "kb/tasks/other.md")
	h.advance(t)
	p.round(t)
	h.settle(t)
	s = h.stats()
	require.Equal(t, 1, s.Merges, "the new peer tip is retried and merges cleanly")
	require.Contains(t, h.content(t, cHostAgent, "kb/tasks/shared.md"), "peer's version",
		"F1: after the peer's own LocalWins merge, the peer's version lands on the host")
}

// T-B6: once the merger has merged a peer's fact into the host's agent
// branch, the host's own triggers fire on it, with source "merged" (signed
// by another key). The test never kicks the merger: the push must.
//
// SABOTAGE: consensusKicks returning false for pushed branches (kick only on
// the host's own commits) → nothing merges, no fire → red.
func TestConsensus_HostTriggersFireOnMergedFacts(t *testing.T) {
	h := newConsensusHost(t, triggerOntology(autoAttrs, trig("seen", "[learn, update]", "", "")), hostOpts{})
	p := newConsensusPeer(t, h.url)
	p.write(t, "kb/tasks/peer.md", "peer")
	p.round(t)

	hostFires := func() []store.TriggerFire {
		rows, err := h.svc(t).Triggers().RecentTriggerFires(context.Background(), cHostAgent, 1000)
		require.NoError(t, err)
		var out []store.TriggerFire
		for _, f := range rows {
			if f.Trigger == "seen" && f.Path == "kb/tasks/peer.md" {
				out = append(out, f)
			}
		}
		return out
	}
	require.Eventually(t, func() bool { return len(hostFires()) > 0 }, 20*time.Second, 20*time.Millisecond,
		"the host's trigger fires on the merged peer fact")
	for _, f := range hostFires() {
		require.Equal(t, "merged", f.Source)
	}
}

// T-B7: the push POST returns while the merge is still held: the merge runs
// on the merger's goroutine, never inside receive-pack's register.
//
// SABOTAGE: running the merger inside ri.onCommit (m.run(ctx) instead of the
// kick) → the push blocks on the held merge → red.
func TestConsensus_PushLatencyUnchanged(t *testing.T) {
	h := newConsensusHost(t, triggerOntology(autoAttrs), hostOpts{})
	p := newConsensusPeer(t, h.url)
	p.write(t, "kb/tasks/peer.md", "peer")

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setConsensusHooks(t, consensusHooks{beforeMerge: func(string) {
		once.Do(func() { close(entered) })
		<-release
	}})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		p.round(t)
	}()
	select {
	case <-pushed:
	case <-time.After(10 * time.Second):
		unblock()
		t.Fatal("the push did not return while the merge was held")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the merger never reached the merge")
	}
	require.False(t, h.has(t, cHostAgent, "kb/tasks/peer.md"), "the merge is still held")
	unblock()
	require.Eventually(t, func() bool { return h.has(t, cHostAgent, "kb/tasks/peer.md") }, 10*time.Second, 10*time.Millisecond)
}

// T-B8 [N2]: a pushed branch is waiting while consensus is off; the host's
// consensus branch then advances to an ontology with `consensus: auto`, and
// the branch is merged with no new push. The test never kicks the merger.
//
// SABOTAGE: consensusKicks only for `agent/*` branches (a peer's push) → the
// consensus branch's advance never kicks → red.
func TestConsensus_EnabledAtRuntime(t *testing.T) {
	h := newConsensusHost(t, triggerOntology(""), hostOpts{})
	p := newConsensusPeer(t, h.url)
	p.write(t, "kb/tasks/peer.md", "peer")
	p.round(t)
	h.settle(t)
	require.False(t, h.has(t, cHostAgent, "kb/tasks/peer.md"), "off: nothing merged")

	h.setOntology(t, triggerOntology(autoAttrs)) // agent commit, then the consensus branch advances
	require.Eventually(t, func() bool { return h.has(t, cHostAgent, "kb/tasks/peer.md") }, 5*time.Second, 10*time.Millisecond,
		"switching auto on at the consensus branch merges the waiting branch")
}

// T-B9: a read-only server has no merger at all, whatever its ontology says.
// (A subscription has none either; it has no agent branch, and
// isPushedBranch already answers no for every branch there.)
//
// SABOTAGE: dropping `!b.cfg.ReadOnly` from the merger's guard in build() →
// a merger exists → red.
func TestConsensus_ReadOnlyHasNoMerger(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home, OntologyRoot: "kb", ReadOnly: true},
		AgentBranch:           cHostAgent,
		KeyPath:               filepath.Join(home, "agent.key"),
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { m.Close() })
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "kb")
	require.Nil(t, ri.consensus, "a read-only server builds no consensus merger")
	ri.consensusKick() // nil-safe: a pushed-branch commit on a read-only server kicks nothing
}
