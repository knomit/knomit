package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"knomit/internal/store/gitserve"

	"knomit/internal/store/verify"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store/merge"
)

// mergeFn is MergePushed or MergeConsensus, as the merge-time guard tests
// call them.
type mergeFn func(s *Service, ctx context.Context, src, dst string, tip plumbing.Hash) (merge.AgentReconcileResult, error)

var guardedMerges = map[string]mergeFn{
	"MergePushed": func(s *Service, ctx context.Context, src, dst string, tip plumbing.Hash) (merge.AgentReconcileResult, error) {
		return s.MergePushed(ctx, src, dst, tip, "")
	},
	"MergeConsensus": func(s *Service, ctx context.Context, src, dst string, tip plumbing.Hash) (merge.AgentReconcileResult, error) {
		return s.MergeConsensus(ctx, src, dst, tip)
	},
}

// branchWith creates branch from `from` on the host and commits files on it,
// returning the new tip.
func (h *pushHost) branchWith(branch, from string, kv ...string) plumbing.Hash {
	h.t.Helper()
	ctx := context.Background()
	require.NoError(h.t, h.svc.CreateBranch(ctx, branch, from))
	fi := h.svc.Facts()
	for i := 0; i+1 < len(kv); i += 2 {
		_, _, err := fi.WriteFileExactForTest(ctx, branch, kv[i], kv[i+1], "write "+kv[i], "edit", "")
		require.NoError(h.t, err)
	}
	return h.ref(branch)
}

// P1 merge-time guard (the authority): for both routes from a pushed branch
// into the host's agent branch, a branch that REVERTS the agent branch's
// .knomit/ change is refused with *PrivatePathMergeError naming the path and
// moves nothing; a branch with a kb/ change merges. The bad branch is tried
// first and does not stop the good one: exactly one merge commit lands.
func TestMergeGuard_RefusesPrivateRevertMergesKB(t *testing.T) {
	for name, mergeCall := range guardedMerges {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			h := newPushHost(t, verify.VerifyOff, 1)
			h.svc.SetSigner(newTestSigner(t))
			old := h.branchFiles(hostBranch)[fact.OntologyFile]
			fork := hostBranch + "-fork"
			require.NoError(t, h.svc.CreateBranch(ctx, fork, hostBranch)) // pre-edit fork point

			// The host's agent branch changes its ontology.
			newer := kbOntology(verify.VerifyOff) + "# host edit\n"
			_, _, err := h.svc.Facts().WriteFileExactForTest(ctx, hostBranch, fact.OntologyFile, newer, "ontology", "edit", "")
			require.NoError(t, err)

			// bad: forked AFTER the edit, puts the old ontology back (a revert).
			bad := h.branchWith("agent/bad-22222222", hostBranch,
				"kb/notes/bad.md", testFactBody("bad", 0.9, nil), fact.OntologyFile, old)
			// good: forked BEFORE the edit (old ontology, unchanged), adds a kb fact.
			good := h.branchWith("agent/good-33333333", fork, "kb/notes/good.md", testFactBody("good", 0.9, nil))

			agentTip := h.ref(hostBranch)
			knomitBefore := knomitTreeHash(t, h, hostBranch)
			merged := 0
			var refused []*merge.PrivatePathMergeError
			for _, b := range []struct {
				name string
				tip  plumbing.Hash
			}{{"agent/bad-22222222", bad}, {"agent/good-33333333", good}} {
				res, err := mergeCall(h.svc, ctx, b.name, hostBranch, b.tip)
				var pe *merge.PrivatePathMergeError
				switch {
				case err == nil:
					require.Equal(t, merge.ModeMerge, res.Mode)
					merged++
				case errors.As(err, &pe):
					refused = append(refused, pe)
				default:
					t.Fatalf("%s: unexpected error %v", b.name, err)
				}
				if b.name == "agent/bad-22222222" {
					require.Equal(t, agentTip, h.ref(hostBranch), "a refused merge moves nothing")
				}
			}
			require.Equal(t, 1, merged, "the good branch merges")
			require.Len(t, refused, 1, "the bad branch is refused")
			require.Equal(t, "agent/bad-22222222", refused[0].Src)
			require.Equal(t, []string{fact.OntologyFile}, refused[0].Paths)
			require.Equal(t, knomitBefore, knomitTreeHash(t, h, hostBranch), ".knomit/ unchanged")
			require.Equal(t, newer, h.branchFiles(hostBranch)[fact.OntologyFile])
			got := h.branchFiles(hostBranch)
			require.Contains(t, got, "kb/notes/good.md")
			require.NotContains(t, got, "kb/notes/bad.md")
		})
	}
}

// The guard judges the RESULT, not who decided it: an explicit side that
// takes the peer's version of a conflicting .knomit path is still refused.
func TestMergeGuard_SideCannotOverrideIt(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, verify.VerifyOff, 1)
	h.svc.SetSigner(newTestSigner(t))
	peer := h.branchWith("agent/peer-44444444", hostBranch, fact.OntologyFile, kbOntology(verify.VerifyOff)+"# peer\n")
	_, _, err := h.svc.Facts().WriteFileExactForTest(ctx, hostBranch, fact.OntologyFile, kbOntology(verify.VerifyOff)+"# host\n", "o", "edit", "")
	require.NoError(t, err)
	tip := h.ref(hostBranch)
	_, err = h.svc.MergePushed(ctx, "agent/peer-44444444", hostBranch, peer, merge.ResolveSrc)
	require.ErrorIs(t, err, merge.ErrPrivatePathMerge)
	require.Equal(t, tip, h.ref(hostBranch))
}

// F4: a commit with no merge base on the upstream (a root commit with its own
// history) is compared with the EMPTY tree, so carrying any private path
// refuses it; the same root commit without one is accepted.
func TestReceivePack_ClosureUnrelatedHistoryEmptyTree(t *testing.T) {
	h := newPushHost(t, verify.VerifyOff, 1)
	fp, branch := pusherOf(t, newTestSigner(t))
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch})
	p := newPeer(t, h.srv.URL)
	head := p.head()

	root := p.commitAs("peer-abcd1234", []*object.Commit{head}, nil,
		map[string]string{fact.OntologyFile: p.files(head)[fact.OntologyFile], "kb/notes/r.md": testFactBody("r", 0.9, nil)})
	root = p.reparent(root) // no parents: unrelated history
	p.setBranch("work", root)
	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
	require.Error(t, err)
	require.Contains(t, err.Error(), "knomit: commit "+root.Hash.String()+" changes .knomit/ontology.yaml relative to main")
	require.Equal(t, plumbing.ZeroHash, h.ref(branch))

	plain := p.reparent(p.commitAs("peer-abcd1234", []*object.Commit{head}, nil,
		map[string]string{"kb/notes/r.md": testFactBody("r", 0.9, nil)}))
	p.setBranch("work", plain)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
	require.Equal(t, plain.Hash, h.ref(branch))
}

// reparent rewrites c as a root commit (no parents), unsigned.
func (p *peer) reparent(c *object.Commit) *object.Commit {
	p.t.Helper()
	n := *c
	n.ParentHashes = nil
	n.PGPSignature = ""
	st := p.repo.Storer
	o := st.NewEncodedObject()
	require.NoError(p.t, n.Encode(o))
	h, err := st.SetEncodedObject(o)
	require.NoError(p.t, err)
	got, err := object.GetCommit(st, h)
	require.NoError(p.t, err)
	return got
}
