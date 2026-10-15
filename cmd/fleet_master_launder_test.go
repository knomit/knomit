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

// The laundering attack on a fleet master with an origin (PR #523 review S3),
// end to end over the real stack. A member branch X on the forge edits
// another member's record; the master's round fetches X and refuses it, so X
// is HELD by the master but never admitted. An enrolled instance then pushes
// to the master's own /git an unsigned, clean merge M of X and main: if the
// push were accepted, receive-pack would judge only M (it skips commits the
// host holds), admit it, and the merger would carry X into main. The fleet
// copy with an origin accepts no push (repos.Manager.FleetPushRefusal), so the
// push is refused, and after the next round main still does not hold X.
//
// SABOTAGE: drop the fleetRefusal case in web.pushPolicy -> the push is
// accepted, agent/launder is registered, the merger merges it and the forge's
// main holds X's file, red.
func TestFleetMaster_ForgeCopyRefusesPushesSoARefusedTipCannotBeLaundered(t *testing.T) {
	ctx := context.Background()
	f := pkitest.New(t)
	dir := t.TempDir()

	// The forge: main holds the fleet ontology and the fleet's root and CRL.
	bare := filepath.Join(dir, "fleet.git")
	missionGit(t, dir, "init", "-q", "--bare", "--initial-branch=main", bare)
	seed := filepath.Join(dir, "seed")
	missionGit(t, dir, "clone", "-q", bare, seed)
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
		if err := os.MkdirAll(filepath.Join(seed, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seed, p), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missionGit(t, seed, "add", "-A")
	missionGit(t, seed, "commit", "-q", "-m", "fleet")
	missionGit(t, seed, "push", "-q", "origin", "main")
	missionGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	forgeURL := fileuri.New(bare)

	// The master B: the fleet's root key in <home>/fleet, a clone of the
	// forge through the fleet door, serving /git over mTLS.
	bCfg := config.Defaults()
	bCfg.Home = filepath.Join(dir, "bravo")
	bCfg.OntologyRoot = "kb"
	bCfg.LocalOriginRoot = dir
	bCfg.TLS = config.TLSConfig{Addr: "127.0.0.1:0", Dir: filepath.Join(bCfg.Home, "pki")}
	if err := os.MkdirAll(filepath.Join(bCfg.Home, pki.FleetRootDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{pki.RootKeyFile, pki.RootCertFile} {
		b, err := os.ReadFile(filepath.Join(f.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bCfg.Home, pki.FleetRootDirName, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bKey, _ := pkitest.NewKey(t)
	bMgr := repos.New(ctx, repos.Deps{Cfg: bCfg, KeyPath: bKey, Signer: keySigner(t, bKey), AgentID: "bravo",
		AgentBranch: "agent/bravo", Machine: repos.Options{Synchronous: true}})
	if err := bMgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bMgr.Close() })
	bSrv := &web.Server{Manager: bMgr, GitHandler: web.GitRemoteHandler(bMgr), APIOnly: true,
		Auth: bCfg.Auth, Grants: auth.NewSQLGrants(bMgr.ControlDB())}
	f.Install(t, f.Enroll(t, "bravo@"+pki.AgentEmailDomain, bKey), bCfg.TLS.Dir)
	_, tlsAddr := serveFleetNode(t, bSrv.Handler(), bKey, bCfg.TLS)
	st, err := bMgr.RegisterFleet(ctx, forgeURL, "", "")
	if err != nil {
		t.Fatalf("mount the fleet: %v", err)
	}
	ri := bMgr.Get(st.FleetRepo)
	if ri == nil {
		t.Fatalf("no fleet repo %q", st.FleetRepo)
	}
	// One master round: Sync re-enters on a re-attach of the same origin and,
	// Synchronous, runs exactly one round.
	round := func() {
		t.Helper()
		if _, err := bMgr.Send(ctx, ri, repos.AttachOrigin(repos.OriginSpec{URL: forgeURL})); err != nil {
			t.Fatalf("round: %v", err)
		}
	}
	round()

	// X: an unsigned commit on the forge editing another member's record.
	evil := "kb/members/bravo/evil.md"
	missionGit(t, seed, "fetch", "-q", "origin")
	missionGit(t, seed, "checkout", "-q", "-b", "agent/x", "origin/main")
	if err := os.MkdirAll(filepath.Join(seed, filepath.Dir(evil)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, evil), []byte("---\nkind: pragmatic\ntype: policy\n---\n# evil\n\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missionGit(t, seed, "add", "-A")
	missionGit(t, seed, "commit", "-q", "-m", "X")
	missionGit(t, seed, "push", "-q", "origin", "agent/x")

	round() // the master fetches X and refuses it
	if r, ok := bMgr.FleetRefusals()["agent/x"]; !ok || r.Reason == "" {
		t.Fatalf("fixture: X is not refused: %+v", bMgr.FleetRefusals())
	}

	// M: an unsigned clean merge of X and main, pushed to the master's /git
	// by an enrolled instance. The pack carries M ALONE: the master already
	// holds X (it fetched it), so receive-pack's walk would never reach X.
	_, _, mallory, _ := newFleetFetcherHome(t, f, "mallory")
	missionGit(t, seed, "fetch", "-q", "origin")
	missionGit(t, seed, "checkout", "-q", "-b", "launder", "origin/main")
	missionGit(t, seed, "merge", "-q", "--no-ff", "--no-edit", "agent/x")
	repo, err := gogit.PlainOpen(seed)
	if err != nil {
		t.Fatal(err)
	}
	m, err := repo.ResolveRevision("launder")
	if err != nil {
		t.Fatal(err)
	}
	x, _ := repo.ResolveRevision("agent/x")
	t.Logf("X = %s, M = %s", x, m)
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, repo.Storer, false).Encode([]plumbing.Hash{*m}, 0); err != nil {
		t.Fatal(err)
	}
	req := packp.NewReferenceUpdateRequest()
	req.Capabilities.Set(capability.ReportStatus)
	req.Commands = []*packp.Command{{Name: "refs/heads/agent/launder", Old: plumbing.ZeroHash, New: *m}}
	req.Packfile = io.NopCloser(&pack)
	var body bytes.Buffer
	if err := req.Encode(&body); err != nil {
		t.Fatal(err)
	}
	resp, err := f.Client(t, mallory).Post("https://"+tlsAddr+"/git/"+ri.Name()+"/git-receive-pack",
		"application/x-git-receive-pack-request", &body)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("receive-pack: %d %q", resp.StatusCode, reply)
	if !strings.Contains(string(reply), "has an origin") {
		t.Errorf("the push to the master's copy was not refused as a fleet copy with an origin: %q", reply)
	}

	round()
	time.Sleep(200 * time.Millisecond) // give the merger a chance it must not take
	round()
	if exec.Command("git", "-C", bare, "cat-file", "-e", "main:"+evil).Run() == nil {
		t.Error("X reached the fleet's main")
	}
	if err := ri.WithRead(func(s *store.Service) {
		if tip, err := s.Branches().HeadCommit(ctx, "agent/launder"); err == nil && tip != "" {
			t.Errorf("agent/launder exists on the master at %s", tip)
		}
		if tip, err := s.Branches().HeadCommit(ctx, "agent/x"); err == nil && tip != "" {
			t.Fatalf("the refused agent/x is a local branch at %s", tip)
		}
	}); err != nil {
		t.Fatal(err)
	}
}
