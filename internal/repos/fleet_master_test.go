package repos

import (
	"context"
	"crypto/ed25519"
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
func round(t *testing.T, ri *RepoInstance) {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	runReconcileLoop(context.Background(), svc, ri.hub, ri.remoteEdgeSink(), ri.Name(), ri.agentBranch,
		makeRemoteAuthFn(config.RemoteAuthConfig{}, ""), ri.env.cfg.LocalOriginRoot, false, nil, nil, nil, nil, nil, ri.peers, true)
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
	// merges. (The bootstrap clone itself copies every branch the forge had.)
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

// The master pushes main WITHOUT force: a main written on the forge after the
// master's fetch makes the push a non-fast-forward, reported as the round's
// ordinary push error, and the forge keeps the other main. The next round
// fetches it, merges it into the master's branch and publishes a main that
// contains it.
// SABOTAGE: push main with "+" (force) in remoteIndex.push -> the forge's
// commit is overwritten and no error is recorded, red.
func TestFleetMaster_MainPushIsNotForcedAndConverges(t *testing.T) {
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
	round(t, f.mri)
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
				reason, err := svc.AdmitPeerBranch(context.Background(), "agent/alpha", ri.AgentBranch(), tip)
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
