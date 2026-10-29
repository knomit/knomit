package manage

import (
	"context"
	"errors"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/storetest"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
	"path/filepath"
	"testing"
)

// The vetted branch is applied only to the origin URL it was vetted against,
// and a refused change leaves the git remote matching what control.db
// records. setOriginUpstream rewrites the refspec before the control.db write
// (the ordering it inherited from SetUpstreamBranch), so a refused write must
// put the git remote back; since the refusal means another write won, back
// means to THAT write's origin, not to what the closure read.
//
// Three races, each forced by writing control.db directly while the live
// (injected) origin still reads as the vetted URL:
//  1. vetted against another URL than the one stored: refused, nothing moves;
//  2. an attach stored R2 after the check: refused, the git remote follows R2;
//  3. a detach removed the origin after the check: refused, no git remote.
//
// SABOTAGE: drop `AND url = ?` from Origins.SetBranch -> step 1 stores ybranch
// -> red; restore `existing` instead of the recorded origin -> the git remote
// stays on the stale URL in step 2 and survives in step 3 -> red.
func TestSetOriginUpstream_AppliesOnlyToTheVettedURL(t *testing.T) {
	originsRoot := t.TempDir()
	m, ri := newControlDBManager(t, originsRoot)
	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	if _, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{
		URL: url, Branch: "main", AuthMethod: "none",
	})); err != nil {
		t.Fatalf("attach: %v", err)
	}
	gitRemote := func() (map[string]bool, []string) {
		var rs, urls []string
		ri.WithRead(func(svc *store.Service) {
			rs = storetest.FetchRefspecs(svc, "origin")
			urls = storetest.RemoteURLs(svc, "origin")
		})
		got := make(map[string]bool, len(rs))
		for _, s := range rs {
			got[s] = true
		}
		return got, urls
	}
	const (
		mainSpec = "+refs/heads/main:refs/remotes/origin/main"
		ySpec    = "+refs/heads/ybranch:refs/remotes/origin/ybranch"
	)
	agentSpec := "+refs/heads/" + ri.AgentBranch() + ":refs/remotes/origin/" + ri.AgentBranch()

	// 1. Vetted against another URL than the one stored.
	err := setOriginUpstream(context.Background(), m, ri, "https://vetted.example/kb.git", "ybranch")
	if !errors.Is(err, control.ErrOriginChanged) {
		t.Fatalf("step 1: a changed origin must be refused with ErrOriginChanged; got %v", err)
	}
	if stored, _ := m.Origins().Get(ri.UID()); stored == nil || stored.Branch != "main" {
		t.Fatalf("step 1: the stored branch moved: %+v", stored)
	}
	if specs, urls := gitRemote(); specs[ySpec] || !specs[mainSpec] || !specs[agentSpec] || len(urls) != 1 || urls[0] != url {
		t.Fatalf("step 1: the git remote must be back on %s main; got %v %v", url, specs, urls)
	}

	// 2. An attach stored R2 between the check and the write.
	const r2 = "https://r2.example/kb.git"
	if err := m.Origins().Set(ri.UID(), control.Origin{URL: r2, Branch: "main", AuthMethod: "none"}); err != nil {
		t.Fatalf("Origins().Set: %v", err)
	}
	err = setOriginUpstream(context.Background(), m, ri, url, "ybranch")
	if !errors.Is(err, control.ErrOriginChanged) {
		t.Fatalf("step 2: the compare-and-set must refuse a record that moved; got %v", err)
	}
	if stored, _ := m.Origins().Get(ri.UID()); stored == nil || stored.URL != r2 || stored.Branch != "main" {
		t.Fatalf("step 2: the branch vetted against %s landed on %s: %+v", url, r2, stored)
	}
	if specs, urls := gitRemote(); specs[ySpec] || !specs[mainSpec] || len(urls) != 1 || urls[0] != r2 {
		t.Fatalf("step 2: the git remote must follow the attach that won (%s main); got %v %v", r2, specs, urls)
	}

	// 3. A detach removed the origin between the check and the write.
	if err := m.Origins().Delete(ri.UID()); err != nil {
		t.Fatalf("Origins().Delete: %v", err)
	}
	err = setOriginUpstream(context.Background(), m, ri, url, "ybranch")
	if !errors.Is(err, control.ErrOriginChanged) {
		t.Fatalf("step 3: a removed origin must be refused with ErrOriginChanged; got %v", err)
	}
	if specs, urls := gitRemote(); len(specs) != 0 || len(urls) != 0 {
		t.Fatalf("step 3: the git remote must follow the detach that won (none); got %v %v", specs, urls)
	}
}

// Every origin write goes through the manager's control.db tenant, which
// Manager.Close nils — and whose methods dereference their crypt on the first
// line. cmd/serve.go's 5s shutdown deadline lets a request outlive the tenant
// it is about to write to, so these must ASK for it and refuse, not dereference
// and panic into the crash-bundle path.
//
// The stopped manager here is one that was never started, which is the same
// nil-tenant state Close leaves behind. ri is nil on purpose: the guard has to
// fire before anything downstream is touched, and a setOriginUpstream that
// reached ri.WithRead first would panic on it.
func TestOriginWrites_RefuseAStoppedManager(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
	})
	calls := []struct {
		name string
		run  func() error
	}{
		{"setOriginUpstream", func() error {
			return setOriginUpstream(context.Background(), m, nil, "", "main")
		}},
	}
	for _, c := range calls {
		err := c.run()
		if !errors.Is(err, repos.ErrManagerStopped) {
			t.Errorf("%s against a stopped manager: got %v, want %v", c.name, err, repos.ErrManagerStopped)
		}
	}
}
