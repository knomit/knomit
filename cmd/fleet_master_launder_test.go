package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web"
)

// The laundering attack on a fleet copy that HOLDS a member commit nobody
// admitted (PR #523 review S3, PR #524 review B1), end to end over the real
// stack. A member branch X on the forge edits another member's record. The
// copy at B holds X's objects without having admitted them: B's master round
// fetched X and refused it, or B's bootstrap clone copied X when B was a
// member. An enrolled instance then pushes to B's /git an unsigned, clean
// merge M of X and main, in a pack that carries M ALONE. Receive-pack used to
// stop its walk at every commit the host holds, so it judged only M (no
// content of its own, admitted) and the merger carried X into main.
//
// The tests below pin the two layers:
//   - a fleet copy with an origin accepts no push at all (FleetPushRefusal,
//     at the web edge);
//   - with the origin gone (the R3.2 "host the fleet here" switch), the walk
//     stops only at commits reachable from main or an admitted local agent/*
//     branch, so X is judged as if new and the push is refused naming it.
//
// launderFixture is that setup: a forge whose main holds the fleet ontology,
// root and CRL, and an instance B, its /git served over mTLS, with the fleet
// repository mounted through the fleet door.
type launderFixture struct {
	ctx     context.Context
	f       *pkitest.Fleet
	bare    string
	seed    string
	url     string
	home    string
	mgr     *repos.Manager
	ri      *repos.RepoInstance
	tlsAddr string
	evil    string
}

func newLaunderFixture(t *testing.T, master bool) *launderFixture {
	t.Helper()
	ctx := context.Background()
	f := pkitest.New(t)
	dir := t.TempDir()
	lf := &launderFixture{ctx: ctx, f: f, evil: "kb/members/bravo/evil.md"}

	lf.bare = filepath.Join(dir, "fleet.git")
	missionGit(t, dir, "init", "-q", "--bare", "--initial-branch=main", lf.bare)
	lf.seed = filepath.Join(dir, "seed")
	missionGit(t, dir, "clone", "-q", lf.bare, lf.seed)
	ont, err := fact.FleetOntology().Serialize()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{repos.OntologyPath: ont}
	for _, name := range []string{pki.RootCertFile, pki.CRLFile} {
		b, err := os.ReadFile(filepath.Join(f.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		files["artifacts/fleet/"+name] = b
	}
	for p, b := range files {
		if err := os.MkdirAll(filepath.Join(lf.seed, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lf.seed, p), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missionGit(t, lf.seed, "add", "-A")
	missionGit(t, lf.seed, "commit", "-q", "-m", "fleet")
	missionGit(t, lf.seed, "push", "-q", "origin", "main")
	missionGit(t, lf.bare, "symbolic-ref", "HEAD", "refs/heads/main")
	lf.url = fileuri.New(lf.bare)

	cfg := config.Defaults()
	cfg.Home = filepath.Join(dir, "bravo")
	lf.home = cfg.Home
	cfg.OntologyRoot = "kb"
	cfg.LocalOriginRoot = dir
	cfg.TLS = config.TLSConfig{Addr: "127.0.0.1:0", Dir: filepath.Join(cfg.Home, "pki")}
	if master {
		lf.installRoot(t)
	}
	key, _ := pkitest.NewKey(t)
	lf.mgr = repos.New(ctx, repos.Deps{Cfg: cfg, KeyPath: key, Signer: keySigner(t, key), AgentID: "bravo",
		AgentBranch: "agent/bravo", Machine: repos.Options{Synchronous: true}})
	if err := lf.mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lf.mgr.Close() })
	srv := &web.Server{Manager: lf.mgr, GitHandler: web.GitRemoteHandler(lf.mgr), APIOnly: true,
		Auth: cfg.Auth, Grants: auth.NewSQLGrants(lf.mgr.ControlDB())}
	f.Install(t, f.Enroll(t, "bravo@"+pki.AgentEmailDomain, key), cfg.TLS.Dir)
	_, lf.tlsAddr = serveFleetNode(t, srv.Handler(), key, cfg.TLS)
	return lf
}

// installRoot copies the fleet's root key and certificate into B's
// <home>/fleet: B is the master from then on (the root on main matches).
func (lf *launderFixture) installRoot(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lf.home, pki.FleetRootDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{pki.RootKeyFile, pki.RootCertFile} {
		b, err := os.ReadFile(filepath.Join(lf.f.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lf.home, pki.FleetRootDirName, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// register mounts the fleet repository at B (RegisterFleet's clone).
func (lf *launderFixture) register(t *testing.T) {
	t.Helper()
	st, err := lf.mgr.RegisterFleet(lf.ctx, lf.url, "", "")
	if err != nil {
		t.Fatalf("mount the fleet: %v", err)
	}
	if lf.ri = lf.mgr.Get(st.FleetRepo); lf.ri == nil {
		t.Fatalf("no fleet repo %q", st.FleetRepo)
	}
}

// round runs one origin round: Sync re-enters on a re-attach of the same
// origin and, Synchronous, runs exactly one round.
func (lf *launderFixture) round(t *testing.T) {
	t.Helper()
	if _, err := lf.mgr.Send(lf.ctx, lf.ri, repos.AttachOrigin(repos.OriginSpec{URL: lf.url})); err != nil {
		t.Fatalf("round: %v", err)
	}
}

func (lf *launderFixture) detach(t *testing.T) {
	t.Helper()
	if _, err := lf.mgr.Send(lf.ctx, lf.ri, repos.DetachOrigin()); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := lf.ri.WithRead(func(s *store.Service) {
		if r, err := s.Remote().GetRemote("origin"); err != nil || r != nil {
			t.Fatalf("fixture: the origin is still there: %+v, %v", r, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// pushX writes X on the forge: an unsigned commit on agent/x editing
// another member's (bravo's) folder.
func (lf *launderFixture) pushX(t *testing.T) plumbing.Hash {
	t.Helper()
	missionGit(t, lf.seed, "fetch", "-q", "origin")
	missionGit(t, lf.seed, "checkout", "-q", "-b", "agent/x", "origin/main")
	if err := os.MkdirAll(filepath.Join(lf.seed, filepath.Dir(lf.evil)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lf.seed, lf.evil), []byte("---\nkind: pragmatic\ntype: policy\n---\n# evil\n\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missionGit(t, lf.seed, "add", "-A")
	missionGit(t, lf.seed, "commit", "-q", "-m", "X")
	missionGit(t, lf.seed, "push", "-q", "origin", "agent/x")
	return lf.seedRev(t, "agent/x")
}

func (lf *launderFixture) seedRev(t *testing.T, rev string) plumbing.Hash {
	t.Helper()
	repo, err := gogit.PlainOpen(lf.seed)
	if err != nil {
		t.Fatal(err)
	}
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		t.Fatal(err)
	}
	return *h
}

// pushLaunder pushes M, an unsigned clean merge of X and main, to B's /git
// as an enrolled instance (mallory), in a pack that carries M ALONE. It
// returns the receive-pack reply.
func (lf *launderFixture) pushLaunder(t *testing.T) string {
	t.Helper()
	_, _, mallory, _ := newFleetFetcherHome(t, lf.f, "mallory")
	missionGit(t, lf.seed, "fetch", "-q", "origin")
	missionGit(t, lf.seed, "checkout", "-q", "-b", "launder", "origin/main")
	missionGit(t, lf.seed, "merge", "-q", "--no-ff", "--no-edit", "agent/x")
	repo, err := gogit.PlainOpen(lf.seed)
	if err != nil {
		t.Fatal(err)
	}
	m := lf.seedRev(t, "launder")
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, repo.Storer, false).Encode([]plumbing.Hash{m}, 0); err != nil {
		t.Fatal(err)
	}
	req := packp.NewReferenceUpdateRequest()
	req.Capabilities.Set(capability.ReportStatus)
	req.Commands = []*packp.Command{{Name: "refs/heads/agent/launder", Old: plumbing.ZeroHash, New: m}}
	req.Packfile = io.NopCloser(&pack)
	var body bytes.Buffer
	if err := req.Encode(&body); err != nil {
		t.Fatal(err)
	}
	resp, err := lf.f.Client(t, mallory).Post("https://"+lf.tlsAddr+"/git/"+lf.ri.Name()+"/git-receive-pack",
		"application/x-git-receive-pack-request", &body)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("receive-pack: %d %q (M = %s)", resp.StatusCode, reply, m)
	return string(reply)
}

// requireNotLaundered: after rounds (with the origin back) the forge's main
// does not hold X's file, and B has no agent/launder and no local agent/x.
func (lf *launderFixture) requireNotLaundered(t *testing.T) {
	t.Helper()
	lf.round(t)
	time.Sleep(200 * time.Millisecond) // give the merger a chance it must not take
	lf.round(t)
	if exec.Command("git", "-C", lf.bare, "cat-file", "-e", "main:"+lf.evil).Run() == nil {
		t.Error("X reached the fleet's main")
	}
	if err := lf.ri.WithRead(func(s *store.Service) {
		if tip, err := s.Branches().HeadCommit(lf.ctx, "agent/launder"); err == nil && tip != "" {
			t.Errorf("agent/launder exists at %s", tip)
		}
		if tip, err := s.Branches().HeadCommit(lf.ctx, "agent/x"); err == nil && tip != "" {
			t.Errorf("the refused agent/x is a local branch at %s", tip)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// A fleet copy with an origin accepts no push: the master's round fetched X
// and refused it, and the pushed merge is refused at the web edge.
//
// SABOTAGE: drop the fleetRefusal case in web.pushPolicy -> the push reaches
// receive-pack, which now judges X (held, not admitted) and refuses it; the
// "has an origin" assertion goes red, X still stays off main.
func TestFleetMaster_ForgeCopyRefusesPushesSoARefusedTipCannotBeLaundered(t *testing.T) {
	lf := newLaunderFixture(t, true)
	lf.register(t)
	lf.round(t)
	lf.pushX(t)
	lf.round(t) // the master fetches X and refuses it
	if r, ok := lf.mgr.FleetRefusals()["agent/x"]; !ok || r.Reason == "" {
		t.Fatalf("fixture: X is not refused: %+v", lf.mgr.FleetRefusals())
	}
	reply := lf.pushLaunder(t)
	if !strings.Contains(reply, "has an origin") {
		t.Errorf("the push to the master's copy was not refused as a fleet copy with an origin: %q", reply)
	}
	lf.requireNotLaundered(t)
}

// B1, the reviewer's reproduction: the master fetched and refused X, then
// its copy loses its origin (DELETE /repos/{fleet}/origin, the knomit-hosted
// switch). FleetPushRefusal no longer applies, so the push reaches
// receive-pack. X is held but reachable from neither main nor an admitted
// agent branch, so the walk judges it as if it were new: the push is refused
// naming X, and main never holds X.
//
// SABOTAGE: in quarantine.newCommits, stop at every held commit again (drop
// the admitted check) -> the push is accepted ("ok refs/heads/agent/launder"),
// red.
func TestFleetMaster_HubJudgesAHeldRefusedCommitAsNew(t *testing.T) {
	lf := newLaunderFixture(t, true)
	lf.register(t)
	lf.round(t)
	x := lf.pushX(t)
	lf.round(t)
	if r, ok := lf.mgr.FleetRefusals()["agent/x"]; !ok || r.Tip != x.String() {
		t.Fatalf("fixture: X is not refused: %+v", lf.mgr.FleetRefusals())
	}
	lf.detach(t)
	if err := lf.ri.WithRead(func(s *store.Service) {
		if !s.CommitExists(x) {
			t.Fatal("fixture: the hub does not hold X")
		}
	}); err != nil {
		t.Fatal(err)
	}

	reply := lf.pushLaunder(t)
	if strings.Contains(reply, "has an origin") {
		t.Fatalf("fixture: the copy still has an origin: %q", reply)
	}
	if !strings.Contains(reply, "commit "+x.String()+" refused") {
		t.Errorf("the pushed merge was not refused for X (%s): %q", x, reply)
	}
	lf.requireNotLaundered(t)
}

// B1 via a member's bootstrap clone: X was on the forge when B, a member,
// cloned the fleet. A clone made before PR #524 copied every agent/* branch
// with its objects; nobody judged them. (Since S2 the fleet door's clone
// fetches only main and the own branch, so the fixture recreates such a home
// with one fetch of every agent/* branch.) B then becomes the hub: the root
// key is copied in and the origin removed (R3.2 steps 2-3). The pushed merge
// is refused naming X, and main never holds X.
//
// SABOTAGE: as above, stop at every held commit -> accepted, red.
func TestFleetMaster_HubJudgesACommitItsMemberCloneHeld(t *testing.T) {
	lf := newLaunderFixture(t, false)
	x := lf.pushX(t)
	lf.register(t)
	if err := lf.ri.WithRead(func(s *store.Service) {
		if s.CommitExists(x) {
			t.Fatal("fixture: the fleet door's clone fetched another agent's branch (S2)")
		}
		// A home cloned before S2: the wildcard brought every agent/* branch.
		if _, err := s.Remote().SyncWithPeers(lf.ctx, lf.ri.AgentBranch(), nil); err != nil {
			t.Fatal(err)
		}
		if !s.CommitExists(x) {
			t.Fatal("fixture: the member does not hold X")
		}
	}); err != nil {
		t.Fatal(err)
	}
	lf.installRoot(t)
	lf.detach(t)

	reply := lf.pushLaunder(t)
	if strings.Contains(reply, "has an origin") {
		t.Fatalf("fixture: the copy still has an origin: %q", reply)
	}
	if !strings.Contains(reply, "commit "+x.String()+" refused") {
		t.Errorf("the pushed merge was not refused for X (%s): %q", x, reply)
	}
	lf.requireNotLaundered(t)
}
