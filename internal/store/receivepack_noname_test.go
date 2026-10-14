package store

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/pki"
)

// D18/P1b: a branch's NAME carries no ownership. An enrolled pusher whose
// commits are its own (signed by its key, authored as its record's id) may
// push to an agent branch whose name lacks its fp8, misstates it, or looks
// like another member's — every one of these used to be refused by the
// "-<Short(pusher)>" suffix rule. What still binds content to the pusher is
// signer = pusher: the SAME branch, pushed with a commit another enrolled
// member signed, is refused under enforce with rule not-pusher, and the ref
// does not move. A push to main is refused whatever is in it.
func TestReceivePack_BranchNameCarriesNoOwnership(t *testing.T) {
	h := newPushHost(t, VerifyEnforce, 1)
	alpha, beta := newTestSigner(t), newTestSigner(t)
	alphaFP, _ := pusherOf(t, alpha)
	betaFP, _ := pusherOf(t, beta)
	require.NotEqual(t, pki.Short(alphaFP), pki.Short(betaFP), "fixture: distinct fp8s")
	members := []FleetMember{
		{Member: fact.Member{Agent: "alpha", State: fact.MemberActive, Key: alpha.PublicKey()}},
		{Member: fact.Member{Agent: "beta", State: fact.MemberActive, Key: beta.PublicKey()}},
	}
	h.setPolicy(PushPolicy{Pusher: alphaFP, OwnBranch: hostBranch, Members: h.members(members, nil)})
	p := newPeer(t, h.srv.URL)
	base := p.head()

	oneOff := []byte(pki.Short(alphaFP))
	oneOff[7] = "01"[map[bool]int{true: 1, false: 0}[oneOff[7] == '0']]
	for i, dst := range []string{
		"agent/alpha",                      // no fp8 at all (an opaque id)
		"agent/alpha-" + pki.Short(betaFP), // misstates its fp8 (another key's)
		"agent/beta-" + pki.Short(betaFP),  // a name that "looks like" beta's
		"agent/alpha-" + string(oneOff),    // one hex digit off its own fp8
	} {
		t.Run(dst, func(t *testing.T) {
			file := fmt.Sprintf("kb/notes/n-%d.md", i)
			// Refused: a commit BETA signed and authored as beta. beta's record
			// holds that key, so judgeCommit alone accepts it; only
			// signer = pusher refuses it.
			foreign := p.commitAs("beta", []*object.Commit{base}, beta, with(p.files(base), file, testFactBody("b", 0.9, nil)))
			p.setBranch("work", foreign)
			objs := h.objects()
			err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+dst)
			require.Error(t, err)
			require.Contains(t, err.Error(), "refused by verify_signatures=enforce: "+RuleNotPusher)
			require.Equal(t, plumbing.ZeroHash, h.ref(dst), "a refused push creates no branch")
			require.Equal(t, objs, h.objects(), "no object of a refused push is kept")

			// Accepted: the same branch, a commit ALPHA (the pusher) signed.
			own := p.commitAs("alpha", []*object.Commit{base}, alpha, with(p.files(base), file, testFactBody("a", 0.9, nil)))
			p.setBranch("work", own)
			require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+dst))
			require.Equal(t, own.Hash, h.ref(dst), "the pusher's own commit is registered at that branch")
		})
	}

	// main stays refused, with the pusher's own signed commit.
	mainBefore := h.ref("main")
	own := p.commitAs("alpha", []*object.Commit{base}, alpha, with(p.files(base), "kb/notes/m.md", testFactBody("m", 0.9, nil)))
	p.setBranch("work", own)
	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/main")
	require.Error(t, err)
	require.Contains(t, err.Error(), "refs/heads/main is not an agent branch")
	require.Equal(t, mainBefore, h.ref("main"))
}
