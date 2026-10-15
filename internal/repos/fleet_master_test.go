package repos

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// forgeFleet is a fleet hosted on a forge (a bare repository): main holds the
// built-in fleet ontology, the root certificate and CRL, and the leaves of
// the members named in leaves. The master holds the root key and a clone.
type forgeFleet struct {
	dir, bare, url string
	root           *pkitest.Fleet
	master         *Manager
	mri            *RepoInstance
}

// newForgeFleet seeds the bare forge, starts the master (root key in its
// <home>/fleet) and mounts its clone through the fleet door. leaves maps an
// agent id to the key its leaf certifies.
func newForgeFleet(t *testing.T, leaves map[string]ssh.Signer) *forgeFleet {
	t.Helper()
	f := &forgeFleet{dir: t.TempDir(), root: pkitest.New(t)}
	f.bare = filepath.Join(f.dir, "fleet.git")
	files := map[string][]byte{OntologyPath: []byte(fleetOntologyYAML(t))}
	for _, name := range []string{pki.RootCertFile, pki.CRLFile} {
		b, err := os.ReadFile(filepath.Join(f.root.Dir, name))
		require.NoError(t, err)
		files["artifacts/fleet/"+name] = b
	}
	for id, k := range leaves {
		pub := k.PublicKey().(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey)
		leaf, _, err := pki.IssueInstance(f.root.Dir, f.root.Root, pub, id+"@agents.knomit.io", time.Hour)
		require.NoError(t, err)
		files["artifacts/fleet/certs/"+id+".pem"] = leaf
	}
	require.NoError(t, os.MkdirAll(f.bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", f.bare)
	work := t.TempDir()
	runGit(t, "", "clone", f.bare, work)
	for p, b := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, p), b, 0o644))
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "fleet")
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, f.bare, "symbolic-ref", "HEAD", "refs/heads/main")
	f.url = fileuri.New(f.bare)

	f.master = f.manager(t, "master", testsigner.Named("master"))
	installFleetRoot(t, f.master, f.root)
	f.mri = mountFleetByDoor(t, f.master, "fleet", f.url)
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		require.True(t, f.mri.isFleetMaster(svc), "fixture: the root key holder is the master")
	}))
	return f
}

// manager starts an instance named id (agent id and branch agent/<id>) on its
// own home under the fleet's dir.
func (f *forgeFleet) manager(t *testing.T, id string, signer ssh.Signer) *Manager {
	t.Helper()
	home := filepath.Join(f.dir, id)
	require.NoError(t, os.MkdirAll(home, 0o755))
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: f.dir},
		AgentBranch: "agent/" + id,
		AgentID:     id,
		Signer:      signer,
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

// join starts a member and registers it: its record is written on its agent
// branch and pushed to the forge.
func (f *forgeFleet) join(t *testing.T, id string, signer ssh.Signer) (*Manager, *RepoInstance) {
	t.Helper()
	m := f.manager(t, id, signer)
	_, err := m.RegisterFleet(context.Background(), f.url, "", "")
	require.NoError(t, err)
	require.NotEmpty(t, f.rev(t, "agent/"+id), "fixture: %s's branch is on the forge", id)
	return m, m.fleetRepo()
}

// round runs ONE origin round of ri's sync loop, as the background loop does.
func round(t *testing.T, ri *RepoInstance) { t.Helper(); roundOnPush(t, ri, nil) }

// roundOnPush is round with the loop's onPush hook (the fleet state
// machine's: Manager.fleetPushed in production).
func roundOnPush(t *testing.T, ri *RepoInstance, onPush func(string, error)) {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	runReconcileLoop(context.Background(), svc, ri.hub, ri.remoteEdgeSink(), ri.Name(), ri.agentBranch,
		makeRemoteAuthFn(config.RemoteAuthConfig{}, ""), ri.env.cfg.LocalOriginRoot, false, onPush, nil, nil, nil, nil, ri.peers, true)
}

// settleMerger waits for a merger run that STARTED after this call (the
// merger runs on its own goroutine under the Serve stage), as cHost.settle.
func settleMerger(t *testing.T, ri *RepoInstance) {
	t.Helper()
	for i := 0; i < 2; i++ {
		before := ri.consensus.snapshot().Runs
		ri.consensus.kickNow()
		require.Eventually(t, func() bool { return ri.consensus.snapshot().Runs > before }, 10*time.Second, 5*time.Millisecond,
			"the consensus merger never ran")
	}
}

// rev is the forge's commit at ref, "" when absent.
func (f *forgeFleet) rev(t *testing.T, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", f.bare, "rev-parse", "--verify", "-q", "refs/heads/"+ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// onForgeMain reports whether path is in the forge's main.
func (f *forgeFleet) onForgeMain(t *testing.T, path string) bool {
	t.Helper()
	return exec.Command("git", "-C", f.bare, "cat-file", "-e", "main:"+path).Run() == nil
}

func localTip(t *testing.T, ri *RepoInstance, branch string) string {
	t.Helper()
	var tip string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		tip, _ = svc.Branches().HeadCommit(context.Background(), branch)
	}))
	return tip
}

// The master merges on a forge (R-merge): its round fetches every member
// branch, judges each tip with the receive-pack core, promotes the admitted
// one to a local agent/* branch, the consensus merger merges it into the
// master's agent branch, and the next round publishes main = that branch,
// pushed without force. A member with no leaf on main is refused (rule A,
// no-cert): left behind on the forge, never a local branch, never merged,
// its reason kept as the branch's refusal.
// SABOTAGE: drop `if master { peers.admit(...) }` in runReconcileLoop -> no
// local agent/alpha, red. Drop the publish call -> forge main never holds
// alpha's record, red. Make AdmitPeerBranch promote without judging ->
// agent/beta is promoted and merged, red.
func TestFleetMaster_ForgeAdmitsMergesAndPublishesMain(t *testing.T) {
	alpha, beta := testsigner.Named("alpha"), testsigner.Named("beta")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha}) // beta has no leaf
	_, ari := f.join(t, "alpha", alpha)
	f.join(t, "beta", beta)
	alphaTip, betaTip := f.rev(t, "agent/alpha"), f.rev(t, "agent/beta")
	mainBefore := f.rev(t, "main")

	round(t, f.mri)
	require.Equal(t, alphaTip, localTip(t, f.mri, "agent/alpha"), "the admitted tip is a local branch")
	require.Empty(t, localTip(t, f.mri, "agent/beta"), "the refused tip never becomes a local branch")
	refusals := f.mri.peers.Refusals()
	require.Len(t, refusals, 1)
	require.Equal(t, betaTip, refusals["agent/beta"].Tip)
	require.Contains(t, refusals["agent/beta"].Reason, "refused by verify_signatures=enforce")

	settleMerger(t, f.mri)
	st := f.mri.consensus.snapshot()
	require.Equal(t, 1, st.Merges, "the merger merged exactly the admitted branch")
	require.Equal(t, 0, st.Refused)

	round(t, f.mri)
	masterTip := localTip(t, f.mri, "agent/master")
	require.Equal(t, masterTip, f.rev(t, "main"), "the forge's main is the master's agent branch")
	require.NotEqual(t, mainBefore, f.rev(t, "main"))
	require.True(t, f.onForgeMain(t, memberRecordPath("kb", "alpha")), "alpha's record is on main")
	require.False(t, f.onForgeMain(t, memberRecordPath("kb", "beta")), "beta's record is left behind")
	runGit(t, f.bare, "merge-base", "--is-ancestor", mainBefore, "main") // a fast-forward of what was there

	// A member's round fetches main and its own branch only: beta's branch
	// (pushed after alpha cloned) never reaches it, and the master's branch,
	// which moved since, stays as alpha's clone saw it. Its merger never
	// merges. (Nor does its clone: TestFleetMember_CloneHoldsNoOtherAgentsBranch.)
	round(t, ari)
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		require.False(t, ari.isFleetMaster(svc))
		tips, err := svc.Remote().PeerTips(ari.AgentBranch())
		require.NoError(t, err)
		_, hasBeta := tips["agent/beta"]
		require.False(t, hasBeta, "a member's round fetches no other agent branch")
		require.NotEqual(t, f.rev(t, "agent/master"), tips["agent/master"].String(), "the master's branch is not re-fetched")
		main, err := svc.Branches().HeadCommit(context.Background(), "main")
		require.NoError(t, err)
		require.Equal(t, f.rev(t, "main"), main, "the member follows main")
	}))
	settleMerger(t, ari)
	require.Zero(t, ari.consensus.snapshot().Attempts, "a member's merger is inert")
	require.Empty(t, ari.peers.Refusals())
}

// A refused tip is judged once: the next round, with neither the tip nor main
// moved, does not judge it again (the refusal stays, with its time).
// SABOTAGE: drop the `seen` check in fleetPeers.admit -> At changes, red.
func TestFleetMaster_RefusedTipIsNotRejudgedUntilItMoves(t *testing.T) {
	beta := testsigner.Named("beta")
	f := newForgeFleet(t, nil)
	f.join(t, "beta", beta)
	round(t, f.mri)
	first := f.mri.peers.Refusals()["agent/beta"]
	require.NotEmpty(t, first.Reason)
	require.Equal(t, first, f.master.FleetRefusals()["agent/beta"], "the Manager reads the fleet repository's refusals")
	time.Sleep(10 * time.Millisecond)
	round(t, f.mri)
	require.Equal(t, first, f.mri.peers.Refusals()["agent/beta"], "not judged again")
}

// STATED ASSUMPTION this test pins (user ruling 2026-10-15, PR #524 S1):
// the forge's main is trusted; branch protection is the forge admin's job.
// The master adopts the forge's main as it is, unjudged (rewinds included),
// merges it into its agent branch and publishes the result: below, an
// unsigned "write past the branch protection" ends up on main after the next
// round, by design.
//
// And the master pushes main WITHOUT force: a main written on the forge
// after the master's fetch makes the push a non-fast-forward, reported as the
// round's ordinary push error, and the forge keeps the other main. The next
// round fetches it, merges it into the master's branch and publishes a main
// that contains it.
// SABOTAGE: push main with "+" (force) in remoteIndex.push -> the forge's
// commit is overwritten and no error is recorded, red.
func TestFleetMaster_TrustsTheForgesMainAndNeverForcesItsPush(t *testing.T) {
	f := newForgeFleet(t, nil)
	round(t, f.mri) // publish the master's branch once
	require.Equal(t, localTip(t, f.mri, "agent/master"), f.rev(t, "main"))

	// Someone writes main on the forge while the master's round is between
	// its fetch and its push.
	var other string
	setSyncHooks(t, syncHooks{attempt: func(_, step string) {
		if step != "push" || other != "" {
			return
		}
		work := t.TempDir()
		runGit(t, "", "clone", "-q", f.bare, work)
		require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("not the master\n"), 0o644))
		runGit(t, work, "add", "-A")
		runGit(t, work, "commit", "-q", "-m", "a write past the branch protection")
		runGit(t, work, "push", "-q", "origin", "main")
		other = f.rev(t, "main")
	}})
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), f.mri.AgentBranch(), "kb/notes/x.md",
			"---\ntype: observation\n---\n# x\n\nx\n", "x", "created")
		require.NoError(t, err)
	}))
	var pushErr error
	roundOnPush(t, f.mri, func(_ string, err error) { pushErr = err })
	require.ErrorContains(t, pushErr, "non-fast-forward", "the fleet state machine sees the refused main as the round's push error")
	require.NotEmpty(t, other)
	require.Equal(t, other, f.rev(t, "main"), "the forge's main is not overwritten")
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		r, err := svc.Remote().GetRemote("origin")
		require.NoError(t, err)
		require.NotNil(t, r.LastPushStatus)
		require.Equal(t, "error", *r.LastPushStatus)
		require.NotNil(t, r.LastPushError)
		require.Contains(t, *r.LastPushError, "non-fast-forward")
	}))

	round(t, f.mri)
	require.Equal(t, localTip(t, f.mri, "agent/master"), f.rev(t, "main"), "converged")
	runGit(t, f.bare, "merge-base", "--is-ancestor", other, "main")
	require.True(t, f.onForgeMain(t, "kb/notes/x.md"))
}

// The merger's gate on the fleet repository is the master predicate, with or
// without an origin, and `consensus` is not read (the fleet ontology does not
// declare it). With the origin detached (the master is the hub, a
// knomit-hosted fleet) the admitted branch is merged; with the root
// certificate in <home>/fleet replaced by another fleet's (a stray key: not
// the master) nothing is merged.
// SABOTAGE: restore the `consensus: auto` + no-origin gate for the fleet
// repository in consensusMerger.runOnce -> Merges 0 in the hub case, red;
// drop the isFleetMaster check -> the stray case merges, red.
func TestFleetMaster_MergerGateIsTheMasterPredicate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stray  bool
		merges int
	}{
		{name: "master, no origin (hub)", merges: 1},
		{name: "stray root, no origin", stray: true, merges: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alpha := testsigner.Named("alpha")
			f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
			f.join(t, "alpha", alpha)
			tip := plumbing.NewHash(f.rev(t, "agent/alpha"))
			// Fetch alpha's objects, then drop the origin: from here this
			// copy is the hub, and alpha's branch arrives as a push would
			// leave it (AdmitPeerBranch writes the same local branch).
			require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
				_, err := svc.Remote().SyncWithPeers(context.Background(), f.mri.AgentBranch(), nil)
				require.NoError(t, err)
			}))
			_, err := f.master.Send(context.Background(), f.mri, DetachOrigin())
			require.NoError(t, err)
			ri := f.master.Get("fleet")
			if tc.stray {
				installFleetRoot(t, f.master, pkitest.New(t))
			}
			require.NoError(t, ri.WithRead(func(svc *store.Service) {
				r, err := svc.Remote().GetRemote("origin")
				require.NoError(t, err)
				require.Nil(t, r, "fixture: no origin")
				require.Equal(t, !tc.stray, ri.isFleetMaster(svc), "fixture: master or not")
				reason, err := svc.AdmitPeerBranch(context.Background(), "agent/alpha", ri.AgentBranch(), tip, nil)
				require.NoError(t, err)
				require.Empty(t, reason)
			}))
			settleMerger(t, ri)
			st := ri.consensus.snapshot()
			require.Equal(t, tc.merges, st.Merges)
			require.Empty(t, st.Warnings, "nothing about `consensus` or the origin is read")
		})
	}
}

// The fetch-time judge is receive-pack's: the private-path closure runs
// before the signature rules, so a member branch that changes .knomit/ is
// refused for that, not for anything else.
// SABOTAGE: drop the privatePathClosure call in store.AdmitPeerBranch -> the
// refusal names the signature rule instead, red.
func TestFleetMaster_FetchJudgeRunsThePrivatePathClosure(t *testing.T) {
	alpha := testsigner.Named("alpha")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
	f.join(t, "alpha", alpha)
	work := t.TempDir()
	runGit(t, "", "clone", "-q", "-b", "agent/alpha", f.bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, ".knomit", "triggers.yaml"), []byte("triggers: []\n"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "a system file")
	runGit(t, work, "push", "-q", "origin", "agent/alpha")

	round(t, f.mri)
	r := f.mri.peers.Refusals()["agent/alpha"]
	require.Equal(t, f.rev(t, "agent/alpha"), r.Tip)
	require.Contains(t, r.Reason, `cannot be pushed`)
	require.Contains(t, r.Reason, ".knomit/triggers.yaml")
	require.Empty(t, localTip(t, f.mri, "agent/alpha"))
}

// PR 3's gap: create publishes main once; if that publish fails, the agent
// branch is on the forge but main is not. The master's next round publishes
// it even though its own push has nothing to send, and the fleet state
// machine (onPush) sees success only once main is published.
// SABOTAGE: publish only when the own push sent something
// (`err == nil && master && pushResult.Pushed`) -> forge main stays behind, red.
func TestFleetMaster_RoundPublishesAMainCreateFailedToPublish(t *testing.T) {
	f := newForgeFleet(t, nil)
	mainBefore := f.rev(t, "main")
	// As create leaves it after a failed publish: a commit on the agent
	// branch, pushed, and main behind it.
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), f.mri.AgentBranch(), "kb/notes/created.md",
			"---\ntype: observation\n---\n# c\n\nc\n", "c", "created")
		require.NoError(t, err)
		_, err = svc.Remote().Push(context.Background(), f.mri.AgentBranch(), nil)
		require.NoError(t, err)
	}))
	tip := localTip(t, f.mri, "agent/master")
	require.Equal(t, tip, f.rev(t, "agent/master"), "fixture: the agent branch is on the forge")
	require.Equal(t, mainBefore, f.rev(t, "main"), "fixture: main was not published")

	calls := 0
	var last error
	roundOnPush(t, f.mri, func(_ string, err error) { calls++; last = err })
	require.Equal(t, 1, calls)
	require.NoError(t, last)
	require.Equal(t, tip, f.rev(t, "main"), "the round published main")
}

// A fetched member tip that edits ANOTHER member's folder is refused, as a
// pushed one is: the fetch judge runs the fleet repository's record rules
// (selfRoot is the fleet's fact root). Today that is rule A's shape check
// (a joiner may touch only members/<own id>/); with the record-owner rule
// (#523) the rule is not-owner. The rule name is not asserted.
// SABOTAGE: pass selfRoot "" to newJudge in judgeIncoming -> rule A does not
// run and even alpha's own record is refused (the control), red.
func TestFleetMaster_FetchedEditOfAnotherMembersFolderIsRefused(t *testing.T) {
	alpha := testsigner.Named("alpha")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
	_, ari := f.join(t, "alpha", alpha)
	// Control: alpha's record alone, under alpha's own folder, is admitted.
	round(t, f.mri)
	admitted := f.rev(t, "agent/alpha")
	require.Equal(t, admitted, localTip(t, f.mri, "agent/alpha"), "fixture: the record alone is admitted")
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ari.AgentBranch(), "kb/members/beta/0000beta.md",
			"---\nkind: pragmatic\ntype: policy\n---\n# beta\n\nnot alpha's to write\n", "beta's folder", "created")
		require.NoError(t, err)
	}))
	round(t, ari) // pushes alpha's branch with the extra commit
	require.NoError(t, exec.Command("git", "-C", f.bare, "cat-file", "-e", "agent/alpha:kb/members/beta/0000beta.md").Run(),
		"fixture: the edit is on the forge")

	round(t, f.mri)
	r := f.mri.peers.Refusals()["agent/alpha"]
	require.Equal(t, f.rev(t, "agent/alpha"), r.Tip)
	require.Contains(t, r.Reason, "refused by verify_signatures=enforce")
	require.Empty(t, localTip(t, f.mri, "agent/alpha"),
		"the branch's latest verdict is a refusal: its local branch, admitted earlier, is demoted (N3)")
	require.NotEqual(t, admitted, r.Tip)
}

// R-visibility applies to a member's FIRST clone too (PR #524 S2): beta's
// branch is on the forge, with a commit main does not hold, before alpha
// joins. Alpha's clone holds main and its own branch, and neither beta's
// tracking ref nor beta's commit. The objects matter, not only the ref: a
// held commit nobody judged is what a laundering merge needs (B1).
// SABOTAGE: drop OwnBranchesOnly from fleetCloneOptions (the wildcard
// bootstrap fetch) -> alpha holds beta's commit and its tracking ref, red.
func TestFleetMember_CloneHoldsNoOtherAgentsBranch(t *testing.T) {
	alpha := testsigner.Named("alpha")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
	work := t.TempDir()
	runGit(t, "", "clone", "-q", f.bare, work)
	runGit(t, work, "checkout", "-q", "-b", "agent/beta")
	require.NoError(t, os.WriteFile(filepath.Join(work, "beta.md"), []byte("beta\n"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "beta's own")
	runGit(t, work, "push", "-q", "origin", "agent/beta")
	beta := plumbing.NewHash(f.rev(t, "agent/beta"))
	require.False(t, beta.IsZero())
	require.NotEqual(t, beta.String(), f.rev(t, "main"))

	_, ari := f.join(t, "alpha", alpha)
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		require.True(t, svc.CommitExists(plumbing.NewHash(f.rev(t, "main"))), "fixture: the clone holds main")
		require.False(t, svc.CommitExists(beta), "a member's clone holds no commit of another agent's branch")
		tips, err := svc.Remote().PeerTips(ari.AgentBranch())
		require.NoError(t, err)
		require.Empty(t, tips, "a member's clone tracks no other agent's branch")
	}))
}

// forgeMainCommit writes (content non-nil) or deletes (nil) path on the
// forge's main, as the forge's admin would: the master adopts it (S1).
func (f *forgeFleet) forgeMainCommit(t *testing.T, path string, content []byte, msg string) {
	t.Helper()
	work := t.TempDir()
	runGit(t, "", "clone", "-q", "-b", "main", f.bare, work)
	if content == nil {
		runGit(t, work, "rm", "-q", path)
	} else {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, path), content, 0o644))
		runGit(t, work, "add", "-A")
	}
	runGit(t, work, "commit", "-q", "-m", msg)
	runGit(t, work, "push", "-q", "origin", "main")
}

// editOwnRecord commits a change to the member's own record on its agent
// branch (signed by the member), and returns nothing: the record stays a
// valid record of the member, only its body grows.
func editOwnRecord(t *testing.T, ri *RepoInstance, id, line string) {
	t.Helper()
	path := memberRecordPath("kb", id)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		got, err := svc.Facts().ReadFact(context.Background(), ri.AgentBranch(), path, nil)
		raw := got.Content
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path, raw+"\n"+line+"\n", "record: "+line, "updated")
		require.NoError(t, err)
	}))
}

// The fetch judge judges EVERY commit of tip ∖ main, not the tip alone (PR
// #524 S3). Alpha's branch carries X (an edit of another member's folder)
// and then Y (an edit of alpha's own record, admissible on its own: the
// control shows a branch of such edits admitted). The tip Y is refused,
// naming X, and no local branch is left.
// SABOTAGE: in store.JudgePeerBranch judge the tip commit only -> Y alone is
// admitted and agent/alpha becomes a local branch, red.
func TestFleetMaster_ARefusedCommitUnderAnAdmissibleTipIsRefused(t *testing.T) {
	alpha := testsigner.Named("alpha")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
	_, ari := f.join(t, "alpha", alpha)
	// Control: the record, then an edit of it, admitted.
	editOwnRecord(t, ari, "alpha", "first edit")
	round(t, ari)
	round(t, f.mri)
	require.Equal(t, f.rev(t, "agent/alpha"), localTip(t, f.mri, "agent/alpha"), "fixture: own-record edits are admitted")
	require.Empty(t, f.mri.peers.Refusals())

	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ari.AgentBranch(), "kb/members/beta/0000beta.md",
			"---\nkind: pragmatic\ntype: policy\n---\n# beta\n\nnot alpha's to write\n", "X: beta's folder", "created")
		require.NoError(t, err)
	}))
	editOwnRecord(t, ari, "alpha", "Y: second edit")
	round(t, ari)
	tip := f.rev(t, "agent/alpha")
	out, err := exec.Command("git", "-C", f.bare, "rev-parse", "agent/alpha~1").Output()
	require.NoError(t, err)
	x := strings.TrimSpace(string(out))
	require.NoError(t, exec.Command("git", "-C", f.bare, "cat-file", "-e", x+":kb/members/beta/0000beta.md").Run(), "fixture: X is the tip's parent")

	round(t, f.mri)
	r := f.mri.peers.Refusals()["agent/alpha"]
	require.Equal(t, tip, r.Tip, "the refusal is of the tip Y")
	require.Contains(t, r.Reason, "commit "+x, "the refusal names X, the commit under the tip")
	require.Empty(t, localTip(t, f.mri, "agent/alpha"), "no local branch is left")
}

// The memo is keyed on (tip, main), and main moving is the enrolment flow
// (PR #524 S4): beta pushes its join branch before its leaf is on main and
// is refused. The issuance then lands on main; beta's tip, unchanged, is
// judged again, admitted and promoted, and the refusal is cleared.
// SABOTAGE: drop main from the memo key (peerKey{tip: ...} only, in
// fleetPeers.admit) -> beta stays refused until it pushes again, red.
func TestFleetMaster_ARefusedTipIsAdmittedWhenItsLeafLandsOnMain(t *testing.T) {
	beta := testsigner.Named("beta")
	f := newForgeFleet(t, nil)
	f.join(t, "beta", beta)
	betaTip := f.rev(t, "agent/beta")
	round(t, f.mri)
	require.Contains(t, f.mri.peers.Refusals()["agent/beta"].Reason, "refused by verify_signatures=enforce", "fixture: no leaf, refused")
	require.Empty(t, localTip(t, f.mri, "agent/beta"))

	pub := beta.PublicKey().(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey)
	leaf, _, err := pki.IssueInstance(f.root.Dir, f.root.Root, pub, "beta@agents.knomit.io", time.Hour)
	require.NoError(t, err)
	f.forgeMainCommit(t, "artifacts/fleet/certs/beta.pem", leaf, "issue beta's leaf")

	round(t, f.mri)
	require.Equal(t, betaTip, f.rev(t, "agent/beta"), "fixture: beta did not push again")
	require.Equal(t, betaTip, localTip(t, f.mri, "agent/beta"), "the same tip is admitted once main holds the leaf")
	require.NotContains(t, f.mri.peers.Refusals(), "agent/beta", "the refusal is cleared")
}

// A member branch deleted on the forge is forgotten by the master (PR #524
// N1): the master's fetch prunes within the peer refspec, so the tracking
// ref goes, and the branch's refusal with it.
// SABOTAGE: drop `opts.Prune = true` in store.fetchOrigin -> the tracking ref
// and the refusal stay, red.
func TestFleetMaster_ABranchDeletedOnTheForgeIsForgotten(t *testing.T) {
	beta := testsigner.Named("beta")
	f := newForgeFleet(t, nil)
	f.join(t, "beta", beta)
	round(t, f.mri)
	require.Contains(t, f.mri.peers.Refusals(), "agent/beta", "fixture: refused")

	runGit(t, f.bare, "branch", "-q", "-D", "agent/beta")
	round(t, f.mri)
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		tips, err := svc.Remote().PeerTips(f.mri.AgentBranch())
		require.NoError(t, err)
		require.NotContains(t, tips, "agent/beta", "the tracking ref is pruned")
		main, err := svc.Branches().HeadCommit(context.Background(), "main")
		require.NoError(t, err)
		require.Equal(t, f.rev(t, "main"), main, "the prune kept main")
	}))
	require.NotContains(t, f.mri.peers.Refusals(), "agent/beta", "the refusal is forgotten")
	require.Equal(t, localTip(t, f.mri, "agent/master"), f.rev(t, "agent/master"), "the own branch is untouched")
}

// A branch admitted before main moved, and refused against the new main,
// is demoted and never merged (PR #524 N3), on both hostings: the merger
// re-judges each local agent/* branch against the current main. Alpha's
// branch is admitted while main holds its leaf; the forge's admin then
// removes the leaf from main (as a revocation would refuse it). The merger,
// held off until then (no root key), re-judges, refuses and demotes it.
// SABOTAGE: drop the demote call in fleetPeers.mayMerge -> the local branch
// stays, red. Drop the admit gate in consensusMerger.runOnce (admit = nil)
// -> alpha is merged, red.
func TestFleetMaster_ARejudgedRefusalDemotesTheAdmittedBranch(t *testing.T) {
	for _, tc := range []struct {
		name string
		hub  bool
	}{
		{name: "forge"},
		{name: "knomit-hosted hub", hub: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			alpha := testsigner.Named("alpha")
			f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
			f.join(t, "alpha", alpha)
			tip := plumbing.NewHash(f.rev(t, "agent/alpha"))
			// Not the master while the branch is admitted: its merger is inert.
			require.NoError(t, os.Remove(filepath.Join(f.master.fleetRootDir(), pki.RootKeyFile)))
			require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
				_, err := svc.Remote().SyncWithPeers(ctx, f.mri.AgentBranch(), nil)
				require.NoError(t, err)
				reason, err := svc.AdmitPeerBranch(ctx, "agent/alpha", f.mri.AgentBranch(), tip, nil)
				require.NoError(t, err)
				require.Empty(t, reason, "fixture: admitted while main holds the leaf")
			}))
			require.Equal(t, tip.String(), localTip(t, f.mri, "agent/alpha"))

			f.forgeMainCommit(t, "artifacts/fleet/certs/alpha.pem", nil, "alpha's leaf leaves main")
			require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
				_, err := svc.Remote().Sync(ctx, f.mri.AgentBranch(), nil)
				require.NoError(t, err)
			}))
			ri := f.mri
			if tc.hub {
				_, err := f.master.Send(ctx, f.mri, DetachOrigin())
				require.NoError(t, err)
				ri = f.master.Get("fleet")
			}
			installFleetRoot(t, f.master, f.root)
			require.NoError(t, ri.WithRead(func(svc *store.Service) {
				require.True(t, ri.isFleetMaster(svc), "fixture: the master again")
				main, err := svc.Branches().HeadCommit(ctx, "main")
				require.NoError(t, err)
				require.Equal(t, f.rev(t, "main"), main, "fixture: main moved here")
			}))

			settleMerger(t, ri)
			st := ri.consensus.snapshot()
			require.Zero(t, st.Merges, "the refused branch is not merged")
			require.Empty(t, localTip(t, ri, "agent/alpha"), "the admitted branch is demoted")
			r := ri.peers.Refusals()["agent/alpha"]
			require.Equal(t, tip.String(), r.Tip)
			require.Contains(t, r.Reason, "refused by verify_signatures=enforce")
		})
	}
}

// One publish path (#518 + PR #524): a forge create whose publish of main
// failed leaves the root holder registering, with main lacking the root, so
// it is not the master yet. The sync loop's round still publishes main
// (publishesFleetMain: the root on the agent branch), the fleet state
// machine reads that round's outcome and enters registered, and fleetPushed
// publishes nothing of its own: the create's publish seam is not called
// again by the round.
// SABOTAGE: gate the round's publish on `master` alone in runReconcileLoop
// -> main stays the README while the state reads registered, red. Put a
// publishFleetMainNow back in fleetPushed's registering arm -> the seam is
// called by the round, red.
func TestFleetMaster_TheLoopRoundPublishesMainForTheCreatorOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := forgeFleetRemote(t, dir)
	readme := gitRev(t, bare, "refs/heads/main")
	m := newFleetHost(t, dir, nil)
	calls := 0
	refuse := true
	setFleetHooks(t, fleetCreateHooks{beforePublish: func() error {
		calls++
		if refuse {
			return errors.New("main is protected")
		}
		return nil
	}})
	st, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.NoError(t, err)
	require.Equal(t, FleetRegistering, st.State, "fixture: main was not published")
	require.Equal(t, readme, gitRev(t, bare, "refs/heads/main"))
	ri := m.fleetRepo()
	require.NotNil(t, ri)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.False(t, ri.isFleetMaster(svc), "fixture: main does not hold the root, so not the master yet")
		require.True(t, ri.publishesFleetMain(svc), "fixture: the root holder publishes")
	}))
	refuse = false
	before := calls

	roundOnPush(t, ri, m.fleetPushed)
	require.Equal(t, before, calls, "the round's own publish is the only one: fleetPushed publishes nothing")
	require.Equal(t, gitRev(t, bare, "refs/heads/"+ri.AgentBranch()), gitRev(t, bare, "refs/heads/main"), "the round published main")
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Empty(t, st.LastError)
}
