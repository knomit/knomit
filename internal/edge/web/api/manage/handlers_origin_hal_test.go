package manage

import (
	"context"
	"errors"
	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/storetest"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
	"path/filepath"
	"testing"
)

// setOriginUpstream shares that ordering and therefore that window: a refspec
// rewritten to the new branch while GetRemote still reports the old one leaves
// reconcileNow reconciling against a refs/remotes/origin/<old> nothing updates
// any more. The restore has to put the refspec back.
//
// The forced failure is a live repo whose control.db row is gone — the injected
// origin still answers GetRemote, so the call gets past its "no origin
// configured" guard and into ConfigureRemote, and Origins.SetBranch then
// updates no rows and says so.
func TestSetOriginUpstream_FailedPersistRestoresTheRefspec(t *testing.T) {
	originsRoot := t.TempDir()
	m, ri := newControlDBManager(t, originsRoot)

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	if _, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{
		URL: url, Branch: "main", AuthMethod: "none",
	})); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := m.Origins().Delete(ri.UID()); err != nil {
		t.Fatalf("Origins().Delete: %v", err)
	}

	if err := setOriginUpstream(context.Background(), m, ri, "develop"); err == nil {
		t.Fatal("SetBranch against a missing row must fail the call")
	}

	var refspecs []string
	ri.WithRead(func(svc *store.Service) { refspecs = storetest.FetchRefspecs(svc, "origin") })
	got := make(map[string]bool, len(refspecs))
	for _, rs := range refspecs {
		got[rs] = true
	}
	if !got["+refs/heads/main:refs/remotes/origin/main"] {
		t.Errorf("the upstream refspec must be restored after a failed persist; got %v", refspecs)
	}
	if got["+refs/heads/develop:refs/remotes/origin/develop"] {
		t.Errorf("the refspec still tracks a branch nothing recorded; got %v", refspecs)
	}
	if !got["+refs/heads/"+ri.AgentBranch()+":refs/remotes/origin/"+ri.AgentBranch()] {
		t.Errorf("the agent-branch refspec must survive the restore; got %v", refspecs)
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
			return setOriginUpstream(context.Background(), m, nil, "main")
		}},
	}
	for _, c := range calls {
		err := c.run()
		if !errors.Is(err, repos.ErrManagerStopped) {
			t.Errorf("%s against a stopped manager: got %v, want %v", c.name, err, repos.ErrManagerStopped)
		}
	}
}
