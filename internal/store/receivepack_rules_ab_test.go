package store

import (
	"bytes"
	"context"
	"regexp"
	"testing"

	"knomit/internal/store/gitserve"

	"knomit/internal/store/fleetrec"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/store/verify"
)

// newFleetPushHost is a knomit host serving a FLEET repository under
// verify_signatures = enforce: the fleet ontology, one active member
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
//	rogue    an active, certified member writing ANOTHER member's folder
//	         (the record-owner rule; receive-pack table only)

// Rules A and B on a knomit host, and the same verdicts from CheckRange over
// a plain checkout of the same commits. A joiner whose leaf artifact is on
// the fleet's main pushes its FIRST record under enforce and is admitted (the
// branch is created on the host); a member whose leaf is revoked is refused;
// a stranger is no-record; an active member writing boss's folder is
// not-owner (the record-owner rule rides the same shared judge).
//
// SABOTAGE: give verify verify.NewJudge(fleet, "") (rule A off on the host) → the
// joiner's push is refused no-record → red.
func TestReceivePack_RulesABMatchCheckRange(t *testing.T) {
	p := newTestFleetPKI(t)
	j, g, z, r := namedSigner(t, "joiner"), namedSigner(t, "gone"), namedSigner(t, "stranger"), namedSigner(t, "rogue")
	jl, _ := p.leaf("joiner", j)
	gl, gs := p.leaf("gone", g)
	rl, _ := p.leaf("rogue", r)
	extra := p.artifacts(p.crl(gs), map[string]string{"joiner": jl, "gone": gl, "rogue": rl})
	extra["kb/members/gone/1.md"] = memberFile("gone", fact.MemberActive, g.PublicKey())
	extra["kb/members/rogue/1.md"] = memberFile("rogue", fact.MemberActive, r.PublicKey())
	h := newFleetPushHost(t, extra)
	fleetCalls := 0
	h.setPolicy(gitserve.PushPolicy{OwnBranch: hostBranch})

	cases := []struct {
		id   string
		path string // the file the commit writes ("" = its own record)
		want string
	}{
		{"joiner", "", ""},
		{"gone", "", verify.RuleRevoked},
		{"stranger", "", verify.RuleNoRecord},
		{"rogue", "kb/members/boss/2.md", verify.RuleNotOwner},
	}
	signers := map[string]ssh.Signer{"joiner": j, "gone": g, "stranger": z, "rogue": r}
	for i, tc := range cases {
		signer := signers[tc.id]
		peer, dir := newDiskPeer(t, h.srv.URL)
		head := peer.head()
		path := tc.path
		if path == "" {
			path = "kb/members/" + tc.id + "/1.md"
		}
		c := peer.commitAs(tc.id, []*object.Commit{head}, signer,
			with(peer.files(head), path, memberFile(tc.id, fact.MemberActive, signer.PublicKey())))
		peer.setBranch("work", c)
		fp, _ := pusherOf(t, signer)
		branch := "agent/case-" + string(rune('a'+i))
		h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: func(context.Context) (fleetrec.Fleet, error) {
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

		v, err := verify.CheckRange(verify.RangeInput{KBDir: dir, Main: "refs/remotes/origin/main", Candidate: c.Hash.String(),
			FleetDir: dir, FleetRev: "refs/remotes/origin/main"})
		require.NoError(t, err)
		require.Equal(t, verify.VerifyEnforce, v.Mode)
		require.Equal(t, 1, v.Checked, tc.id)
		gateRule := ""
		if len(v.Refused) > 0 {
			gateRule = onlyRefusal(t, v).Rule
		}
		require.Equal(t, pushRule, gateRule, "%s: receive-pack and CheckRange must agree", tc.id)
	}
	require.Equal(t, len(cases), fleetCalls, "every push read the fleet")
}

// S2 on a knomit host, the user's ruling pinned: rogue (active, certified)
// pushes two commits to its own folder and their clean merge. The merge's
// signature is (a) present but does not verify → ADMITTED, like an unsigned
// clean merge, because it carries nothing beyond its parents; (b) a VERIFYING
// signature by boss, not the pusher → not-pusher (verify.SignerIsPusher still
// compares a signature that verifies). CheckRange agrees on (a) and admits
// (b): it has no pusher.
//
// SABOTAGE: admit a clean merge only when its signature verifies or is
// absent (`serr == nil || unsigned` in judgeSigner) → (a) is refused
// signature → red.
func TestReceivePack_CleanMergeSignatureStates(t *testing.T) {
	p := newTestFleetPKI(t)
	r, boss := namedSigner(t, "rogue"), namedSigner(t, "boss")
	rl, _ := p.leaf("rogue", r)
	bl, _ := p.leaf("boss", boss)
	extra := p.artifacts(p.crl(), map[string]string{"rogue": rl, "boss": bl})
	extra["kb/members/rogue/1.md"] = memberFile("rogue", fact.MemberActive, r.PublicKey())
	h := newFleetPushHost(t, extra)
	fp, _ := pusherOf(t, r)

	cases := []struct {
		name   string
		mergeK ssh.Signer
		broken bool
		want   string // receive-pack verdict; "" = admitted
		gate   string // CheckRange verdict
	}{
		{"a bad signature", r, true, "", ""},
		{"a verifying signature by another member", boss, false, verify.RuleNotPusher, ""},
	}
	ran := 0
	for i, tc := range cases {
		peer, dir := newDiskPeer(t, h.srv.URL)
		head := peer.head()
		files := peer.files(head)
		a := peer.commitAs("rogue", []*object.Commit{head}, r, with(files, "kb/members/rogue/2.md", "a"))
		b := peer.commitAs("rogue", []*object.Commit{head}, r, with(files, "kb/members/rogue/3.md", "b"))
		m := peer.commitAs("rogue", []*object.Commit{a, b}, tc.mergeK, with(files, "kb/members/rogue/2.md", "a", "kb/members/rogue/3.md", "b"))
		if tc.broken {
			m = brokenSig(t, peer.repo, m)
		}
		v3, err := verify.CheckM3(m)
		require.NoError(t, err)
		require.True(t, v3.OK, "%s: fixture: the merge is M3-clean", tc.name)
		peer.setBranch("work", m)
		branch := "agent/sig-" + string(rune('a'+i))
		h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: func(context.Context) (fleetrec.Fleet, error) {
			return h.svc.FleetAt("main")
		}})

		err = peer.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
		pushRule := ""
		if err != nil {
			mm := pushRuleRE.FindStringSubmatch(err.Error())
			require.NotNil(t, mm, "%s: unexpected push error: %v", tc.name, err)
			pushRule = mm[1]
		}
		require.Equal(t, tc.want, pushRule, tc.name)
		if tc.want == "" {
			require.Equal(t, m.Hash, h.ref(branch), "%s: the admitted branch exists on the host", tc.name)
		} else {
			require.Equal(t, plumbing.ZeroHash, h.ref(branch), "%s: a refused branch is not created", tc.name)
		}

		v, err := verify.CheckRange(verify.RangeInput{KBDir: dir, Main: "refs/remotes/origin/main", Candidate: m.Hash.String(),
			FleetDir: dir, FleetRev: "refs/remotes/origin/main"})
		require.NoError(t, err)
		require.Equal(t, 3, v.Checked, tc.name)
		gateRule := ""
		if len(v.Refused) > 0 {
			gateRule = onlyRefusal(t, v).Rule
		}
		require.Equal(t, tc.gate, gateRule, tc.name)
		ran++
	}
	require.Equal(t, 2, ran)
}
