package store

import (
	"bytes"
	"context"
	"regexp"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
)

// newFleetPushHost is a knomit host serving a FLEET repository under
// verify_signatures = enforce: the fleet preset ontology, one active member
// "boss", and extra (artifacts, more records) on main. Its push policy reads
// the fleet from the host's own main, as repos.Manager.FleetAtMain does when
// the mounted fleet repository is the one being pushed to.
func newFleetPushHost(t *testing.T, extra map[string]string) *pushHost {
	t.Helper()
	files := map[string]string{
		fact.OntologyFile:      fleetEnforceOntology,
		"kb/members/boss/1.md": memberFile("boss", fact.MemberActive, namedSigner(t, "boss").PublicKey()),
	}
	for p, c := range extra {
		files[p] = c
	}
	return newPushHostWith(t, files, 0)
}

// newDiskPeer is newPeer with its clone on disk, so CheckRange can read the
// very same commits as a plain checkout.
func newDiskPeer(t *testing.T, url string) (*peer, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainClone(dir, true, &gogit.CloneOptions{URL: url})
	require.NoError(t, err)
	return &peer{t: t, repo: repo}, dir
}

var pushRuleRE = regexp.MustCompile(`refused by verify_signatures=enforce: ([a-z-]+): `)

// RulesABCases is the scenario table the in-process receive-pack test and the
// `knomit verify ci` test (cmd/verify_signatures_test.go) both pin: agent id
// -> the verdict ("" = admitted). Kept as literals in both places.
//
//	joiner   no record, a valid leaf on main (rule A: the bootstrap)
//	gone     an active record whose leaf is revoked (rule B)
//	stranger no record and no leaf (today's no-record)

// Rules A and B on a knomit host, and the same verdicts from CheckRange over
// a plain checkout of the same commits. A joiner whose leaf artifact is on
// the fleet's main pushes its FIRST record under enforce and is admitted (the
// branch is created on the host); a member whose leaf is revoked is refused;
// a stranger is no-record.
//
// SABOTAGE: give verify newJudge(fleet, "") (rule A off on the host) → the
// joiner's push is refused no-record → red.
func TestReceivePack_RulesABMatchCheckRange(t *testing.T) {
	p := newTestFleetPKI(t)
	j, g, z := namedSigner(t, "joiner"), namedSigner(t, "gone"), namedSigner(t, "stranger")
	jl, _ := p.leaf("joiner", j)
	gl, gs := p.leaf("gone", g)
	extra := p.artifacts(p.crl(gs), map[string]string{"joiner": jl, "gone": gl})
	extra["kb/members/gone/1.md"] = memberFile("gone", fact.MemberActive, g.PublicKey())
	h := newFleetPushHost(t, extra)
	fleetCalls := 0
	h.setPolicy(PushPolicy{OwnBranch: hostBranch})

	cases := []struct {
		id   string
		want string
	}{
		{"joiner", ""},
		{"gone", RuleRevoked},
		{"stranger", RuleNoRecord},
	}
	signers := map[string]ssh.Signer{"joiner": j, "gone": g, "stranger": z}
	for i, tc := range cases {
		signer := signers[tc.id]
		peer, dir := newDiskPeer(t, h.srv.URL)
		head := peer.head()
		c := peer.commitAs(tc.id, []*object.Commit{head}, signer,
			with(peer.files(head), "kb/members/"+tc.id+"/1.md", memberFile(tc.id, fact.MemberActive, signer.PublicKey())))
		peer.setBranch("work", c)
		fp, _ := pusherOf(t, signer)
		branch := "agent/case-" + string(rune('a'+i))
		h.setPolicy(PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: func(context.Context) (Fleet, error) {
			fleetCalls++
			return h.svc.FleetAt("main")
		}})

		err := peer.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
		pushRule := ""
		if err != nil {
			m := pushRuleRE.FindStringSubmatch(err.Error())
			require.NotNil(t, m, "%s: unexpected push error: %v", tc.id, err)
			pushRule = m[1]
		}
		require.Equal(t, tc.want, pushRule, tc.id)
		if tc.want == "" {
			require.Equal(t, c.Hash, h.ref(branch), "%s: the admitted branch exists on the host", tc.id)
		} else {
			require.Equal(t, plumbing.ZeroHash, h.ref(branch), "%s: a refused branch is not created", tc.id)
		}

		v, err := CheckRange(RangeInput{KBDir: dir, Main: "refs/remotes/origin/main", Candidate: c.Hash.String(),
			FleetDir: dir, FleetRev: "refs/remotes/origin/main"})
		require.NoError(t, err)
		require.Equal(t, VerifyEnforce, v.Mode)
		require.Equal(t, 1, v.Checked, tc.id)
		gateRule := ""
		if len(v.Refused) > 0 {
			gateRule = onlyRefusal(t, v).Rule
		}
		require.Equal(t, pushRule, gateRule, "%s: receive-pack and CheckRange must agree", tc.id)
	}
	require.Equal(t, len(cases), fleetCalls, "every push read the fleet")
}
