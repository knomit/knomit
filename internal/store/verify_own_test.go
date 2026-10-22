package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"knomit/internal/store/verify"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/remotesync"
)

// ownHost is a fleet repository on a knomit host whose own agent branch
// (hostBranch) is the master's: boss, an active certified member, authors
// and signs it. agent-x is another certified member. main names onMain as the
// acting master ("" = no marker) and holds a host record.
func ownHost(t *testing.T, onMain string) (*pushHost, ssh.Signer, ssh.Signer) {
	t.Helper()
	p := newTestFleetPKI(t)
	boss, x := namedSigner(t, "boss"), namedSigner(t, "x")
	bl, _ := p.leaf("boss", boss)
	xl, _ := p.leaf("agent-x", x)
	extra := p.artifacts(p.crl(), map[string]string{"boss": bl, "agent-x": xl})
	extra["kb/members/agent-x/1.md"] = memberFile("agent-x", fact.MemberActive, x.PublicKey())
	extra[fact.FleetHostPath] = hostV1
	if onMain != "" {
		extra[fact.FleetMasterPath] = fact.RenderFleetMaster(onMain)
	}
	h := newFleetPushHost(t, extra)
	h.svc.SetSigner(boss)
	h.svc.SetAgentID("boss")
	return h, boss, x
}

// rawCommit puts on branch one commit whose tree is branch's files with kv
// applied (with() semantics), authored as agent and signed by s, moving the
// ref directly: a commit no write path of this instance made.
func (h *pushHost) rawCommit(branch, agent string, s ssh.Signer, kv ...string) plumbing.Hash {
	h.t.Helper()
	parent, err := object.GetCommit(h.svc.rh.Storer(), h.ref(branch))
	require.NoError(h.t, err)
	when := parent.Committer.When.Add(time.Minute)
	c := &object.Commit{
		Author:       object.Signature{Name: agent, Email: agent + "+learn@agents.knomit.io", When: when},
		Committer:    object.Signature{Name: agent, Email: agent + "@agents.knomit.io", When: when},
		Message:      "raw",
		TreeHash:     repoOn(h.t, h.svc.rh.Storer()).tree(with(h.branchFiles(branch), kv...)),
		ParentHashes: []plumbing.Hash{parent.Hash},
	}
	hash, err := vcs.StoreCommit(h.svc.rh.Storer(), s, c)
	require.NoError(h.t, err)
	require.NoError(h.t, h.svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), hash)))
	return hash
}

// write commits kv on the host's own branch through the ordinary write path
// (authored as this instance, signed by its signer); "" deletes.
func (h *pushHost) write(kv ...string) {
	h.t.Helper()
	files, deletes := map[string]string{}, []string{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			deletes = append(deletes, kv[i])
		} else {
			files[kv[i]] = kv[i+1]
		}
	}
	_, _, err := h.svc.Facts().BatchWriteFacts(context.Background(), hostBranch, files, deletes, "own write", "created")
	require.NoError(h.t, err)
}

// The judge of this instance's OWN commits before the fleet's main moves
// (judgeOwnRange, through AdvanceLocalUpstream: the hub's local reconcile).
// Each row is a fresh fleet repository; the master's legitimate writes move
// main, anything else is refused with main unmoved. The row count is
// asserted.
//
// SABOTAGE (one at a time, each against a commit): drop the judgeOwnRange
// call from AdvanceLocalUpstream -> every refused row moves main -> red;
// drop the commitFactsValid pass -> the invalid-record row moves main -> red;
// make ownHostRecord return ok -> the marker-naming-another and the
// host-record-under-another-master rows move main -> red; drop
// verify.SignerIsPusher for own commits -> the rotated-key row moves main -> red;
// judge own commits with entry set -> the master's own host record is
// refused (control rows red); drop judge.ownOntology -> the ontology refresh
// row is refused not-owner -> red.
func TestJudgeOwn_TheMastersOwnCommitsAreJudgedBeforeMainMoves(t *testing.T) {
	const own = "kb/members/boss/1.md"
	policy := func(h *pushHost) string { return h.branchFiles(hostBranch)[own] + "\nnotes: v2\n" }
	rows := []struct {
		name   string
		onMain string
		act    func(h *pushHost, boss, x ssh.Signer)
		want   string // "" = main moves
	}{
		{"its own record edited (control)", "boss", func(h *pushHost, _, _ ssh.Signer) { h.write(own, policy(h)) }, ""},
		{"an invalid member record (not a policy fact)", "boss", func(h *pushHost, _, _ ssh.Signer) {
			// Past the store's write cores (they refuse it at the source): a
			// hand edit, or a writer from before this rule.
			_, err := h.svc.rh.CommitFilesForTest(hostBranch, map[string]string{"kb/members/boss/note.md": "---\ntype: observation\n---\n# note\n\nnot a policy\n"}, "bad")
			require.NoError(h.t, err)
		}, verify.RuleValidation},
		{"the host record, main naming it (control)", "boss", func(h *pushHost, _, _ ssh.Signer) {
			h.write(fact.FleetHostPath, "# knomit fleet host v1\nurl: https://hub2.example/git/fleet\n")
		}, ""},
		{"the host record, main naming nobody (create, a claim) (control)", "", func(h *pushHost, _, _ ssh.Signer) {
			h.write(fact.FleetHostPath, "# knomit fleet host v1\nurl: https://hub2.example/git/fleet\n")
		}, ""},
		{"the marker naming another agent", "boss", func(h *pushHost, _, _ ssh.Signer) {
			h.write(fact.FleetMasterPath, fact.RenderFleetMaster("agent-x"))
		}, verify.RuleHostRecord},
		{"the host record while main names another master", "agent-x", func(h *pushHost, _, _ ssh.Signer) {
			h.write(fact.FleetHostPath, "# knomit fleet host v1\nurl: https://hub2.example/git/fleet\n")
		}, verify.RuleHostRecord},
		{"Host here: the marker naming itself, then the host record, main naming another (control)", "agent-x", func(h *pushHost, _, _ ssh.Signer) {
			h.write(fact.FleetMasterPath, fact.RenderFleetMaster("boss"))
			h.write(fact.FleetHostPath, "# knomit fleet host v1\nurl: https://hub2.example/git/fleet\n")
		}, ""},
		{"stand-down: the marker deleted, main naming it (control)", "boss", func(h *pushHost, _, _ ssh.Signer) {
			h.write(fact.FleetMasterPath, "")
		}, ""},
		{"a fact outside its own folder", "boss", func(h *pushHost, _, _ ssh.Signer) {
			h.write("kb/notes/x.md", "---\nkind: pragmatic\ntype: policy\n---\n# x\n\nx\n")
		}, verify.RuleNotOwner},
		{"the ontology refreshed (control)", "boss", func(h *pushHost, _, _ ssh.Signer) {
			_, err := h.svc.Facts().WriteFact(context.Background(), hostBranch, fact.OntologyFile, fleetEnforceOntology+"# refreshed\n", "refresh", "updated")
			require.NoError(h.t, err)
		}, ""},
		{"its own id signed by a key that is not this instance's", "boss", func(h *pushHost, boss, _ ssh.Signer) {
			h.write(own, policy(h))
			h.svc.SetSigner(namedSigner(h.t, "rotated"))
		}, verify.RuleNotPusher},
		{"a member's commit on its branch that changes the host record", "boss", func(h *pushHost, _, x ssh.Signer) {
			h.rawCommit(hostBranch, "agent-x", x, fact.FleetHostPath, "# knomit fleet host v1\nurl: https://evil.example/git/fleet\n")
		}, verify.RuleHostRecord},
	}
	ran := 0
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			h, boss, x := ownHost(t, r.onMain)
			before := h.ref("main")
			r.act(h, boss, x)
			tip := h.ref(hostBranch)
			require.NotEqual(t, before, tip, "fixture: the branch moved")
			_, err := h.svc.Remote().AdvanceLocalUpstream(context.Background(), hostBranch, "main")
			if r.want == "" {
				require.NoError(t, err)
				require.Equal(t, tip, h.ref("main"), "main followed the branch")
			} else {
				var oe *remotesync.OwnCommitRefusedError
				require.True(t, errors.As(err, &oe), "want the judge's refusal, got %v", err)
				require.ErrorIs(t, err, remotesync.ErrOwnCommitRefused)
				require.Equal(t, r.want, oe.Rule, oe.Text)
				require.Contains(t, err.Error(), remotesync.OwnCommitRefusedText)
				require.Equal(t, before, h.ref("main"), "main did not move")
			}
			ran++
		})
	}
	require.Equal(t, len(rows), ran)
}

// Outside the fleet repository main follows the agent branch unjudged: the
// own-commit judge is the fleet's, and a knowledge base's own writes are
// bound by the MCP write path, not here.
//
// SABOTAGE: make judgeOwnRange judge every repository -> the invalid note
// (and the unsigned host write) holds main -> red.
func TestJudgeOwn_AKnowledgeBaseMainFollowsUnjudged(t *testing.T) {
	h := newPushHostWith(t, map[string]string{fact.OntologyFile: validatedOntology(verify.VerifyEnforce)}, 0)
	_, err := h.svc.rh.CommitFilesForTest(hostBranch, map[string]string{"kb/notes/n.md": note("n")}, "n")
	require.NoError(t, err)
	tip := h.ref(hostBranch)
	_, err = h.svc.Remote().AdvanceLocalUpstream(context.Background(), hostBranch, "main")
	require.NoError(t, err)
	require.Equal(t, tip, h.ref("main"))
}
