package cmd

// F08 PR D: the mission template's knomit-hosted route, run exactly as its
// README writes it, over the real web server and the fleet's cert HTTPS /git.
//
//  1. git init <branch> my-mission; copy examples/mission/. into it (the
//     README's `cp -R examples/mission/. my-mission/`, done in Go so it runs
//     the same on every OS); git add -A; git commit.
//  2. The host clones that repository, then removes its origin
//     (DELETE /api/v1/repos/<repo>/origin).
//  3. A peer clones the repo FROM THE HOST over cert HTTPS.
//
// Run on branches named trunk and master (stock `git init` makes master):
// neither may turn into a hardcoded "main" when the origin goes. The host
// (also after a restart) and the peer must each load all 11 triggers, both
// skills and the consensus / conflicts / sync settings at the tip of the repo's
// consensus branch.
//
// SABOTAGE: store.localConsensusBranch returning the old literal "main" →
// after the DELETE the host's consensus branch is "main" with a zero tip, and
// the peer clone fails ("consensus branch does not exist in this store") →
// red.

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web"
)

func missionGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=m", "GIT_AUTHOR_EMAIL=m@example.invalid",
		"GIT_COMMITTER_NAME=m", "GIT_COMMITTER_EMAIL=m@example.invalid")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// copyTree is `cp -R src/. dst/`: every file, dotfiles included.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// missionTriggers is how many triggers the shipped template declares. It
// has no `do: push` trigger: `sync: {push: realtime}` sends every commit.
const missionTriggers = 11

// requireMissionLoaded asserts ri serves the template from the tip of its
// consensus branch, which must be wantBranch.
func requireMissionLoaded(t *testing.T, who string, ri *repos.RepoInstance, wantBranch string) {
	t.Helper()
	ctx := context.Background()
	if oe := ri.OntologyError(); oe != nil {
		t.Fatalf("%s: ontology error: %v", who, oe)
	}
	// A freshly opened repo lists its triggers once its dispatcher has run.
	var rep repos.TriggerReport
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var err error
		rep, err = ri.TriggerReport(ctx, 0)
		if err != nil {
			t.Fatalf("%s: trigger report: %v", who, err)
		}
		if len(rep.Triggers) >= missionTriggers || time.Now().After(deadline) {
			break
		}
	}
	active := 0
	for _, tr := range rep.Triggers {
		if tr.State == fact.TriggerActive {
			active++
		} else {
			t.Errorf("%s: trigger %s is %s: %s", who, tr.Name, tr.State, tr.Error)
		}
	}
	if active != missionTriggers {
		t.Errorf("%s: %d/%d triggers active", who, active, missionTriggers)
	}
	err := ri.WithRead(func(s *store.Service) {
		ub := s.UpstreamBranch()
		if ub != wantBranch {
			t.Errorf("%s: consensus branch %q, want %q (the branch the repository was created on)", who, ub, wantBranch)
		}
		tip, err := s.Triggers().UpstreamTip(ctx, ub)
		if err != nil || tip.IsZero() {
			t.Errorf("%s: consensus branch %q has no tip (err %v): skills, recipes and settings would read nothing", who, ub, err)
			return
		}
		ents, err := s.Skills().SkillsAt(ctx, tip)
		if err != nil || len(ents) != 2 {
			t.Errorf("%s: skills at the consensus tip: %v (err %v), want 2", who, ents, err)
		}
		onto, err := s.OntologyAt(ctx, ub)
		if err != nil {
			t.Errorf("%s: ontology at %s: %v", who, ub, err)
			return
		}
		cs, _ := fact.ReadConsensus(onto)
		cf, _ := fact.ReadConflicts(onto)
		if cs.Mode != fact.ConsensusAuto || cf.Facts != fact.ConflictsMerge || cf.State != fact.ConflictsConsensus {
			t.Errorf("%s: settings at %s: consensus=%q conflicts=%q/%q", who, ub, cs.Mode, cf.Facts, cf.State)
		}
		// The sync loops read `sync` at this same tip (F21 S2): the claim
		// window is only as short as WINDOW_SECONDS while both keys are
		// realtime there.
		sy, _ := fact.ReadSync(onto)
		if !sy.Valid || !sy.RealtimePush() || !sy.RealtimePull() {
			t.Errorf("%s: sync at %s: push=%q pull=%q valid=%v, want realtime/realtime", who, ub, sy.Push, sy.Pull, sy.Valid)
		}
	})
	if err != nil {
		t.Fatalf("%s: read: %v", who, err)
	}
}

func TestMission_ReadmeKnomitHostedRoute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(wd, "..", "examples", "mission")
	for _, br := range []string{"trunk", "master"} {
		t.Run(br, func(t *testing.T) {
			ctx := context.Background()
			originRoot := t.TempDir()
			repoDir := filepath.Join(originRoot, "my-mission")
			missionGit(t, originRoot, "init", "-b", br, "my-mission")
			copyTree(t, template, repoDir)
			missionGit(t, repoDir, "add", "-A")
			missionGit(t, repoDir, "commit", "-q", "-m", "mission template")

			// The host.
			f := pkitest.New(t)
			hCfg := config.Defaults()
			hCfg.Home = t.TempDir()
			hCfg.OntologyRoot = "kb"
			hCfg.LocalOriginRoot = originRoot
			hCfg.TLS = config.TLSConfig{Addr: "127.0.0.1:0", Dir: filepath.Join(hCfg.Home, "pki")}
			hKey, _ := pkitest.NewKey(t)
			newHost := func() *repos.Manager {
				m := repos.New(ctx, repos.Deps{Cfg: hCfg, KeyPath: hKey, AgentBranch: fleetServerAgent, Machine: repos.Options{Synchronous: true}})
				if err := m.Start(); err != nil {
					t.Fatal(err)
				}
				return m
			}
			hMgr := newHost()
			hSrv := &web.Server{Manager: hMgr, GitHandler: web.GitRemoteHandler(hMgr), APIOnly: true, Auth: hCfg.Auth, Grants: auth.NewSQLGrants(hMgr.ControlDB())}
			f.Install(t, f.Enroll(t, "bravo", pki.RoleInstance, hKey), hCfg.TLS.Dir)
			plain, tlsAddr := serveFleetNode(t, hSrv.Handler(), hKey, hCfg.TLS)

			// Step 2: clone the prepared repository, then remove its origin.
			if _, err := hMgr.Create(ctx, repos.CreateSpec{Name: "mission", Mode: "clone",
				Origin: &repos.OriginSpec{URL: repoDir}}, nil); err != nil {
				t.Fatalf("host clone: %v", err)
			}
			req, _ := http.NewRequest(http.MethodDelete, "http://"+plain+"/api/v1/repos/mission/origin", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("DELETE origin: %d", resp.StatusCode)
			}
			host := hMgr.Get("mission")
			if err := host.WithRead(func(s *store.Service) {
				if r, _ := s.Remote().GetRemote("origin"); r != nil {
					t.Errorf("the origin is still configured: %+v", r)
				}
			}); err != nil {
				t.Fatal(err)
			}
			requireMissionLoaded(t, "host", host, br)

			// Step 3: a peer clones from the host over cert HTTPS.
			pDir, pKey, pMember, _ := newFleetFetcherHome(t, f, "alpha")
			if err := pki.InstallGitTransport(pDir, pKey); err != nil {
				t.Fatalf("install transport: %v", err)
			}
			pMgr := repos.New(ctx, repos.Deps{Cfg: config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
				KeyPath: pKey, AgentBranch: "agent/alpha-" + pki.Short(pMember.Fingerprint()), Machine: repos.Options{Synchronous: true}})
			if err := pMgr.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { pMgr.Close() })
			peer, err := pMgr.Create(ctx, repos.CreateSpec{Name: "mission", Mode: "clone",
				Origin: &repos.OriginSpec{URL: pki.GitScheme + "://" + tlsAddr + "/git/mission", AuthMethod: "cert"}}, nil)
			if err != nil {
				t.Fatalf("peer clone from the host: %v", err)
			}
			requireMissionLoaded(t, "peer", peer, br)

			// The host keeps its consensus branch across a restart: the name is
			// recorded with the repo, not held by the deleted origin row.
			hMgr.Close()
			hMgr2 := newHost()
			t.Cleanup(func() { hMgr2.Close() })
			requireMissionLoaded(t, "host after restart", hMgr2.Get("mission"), br)
		})
	}
}
