package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// D18/P1b review F2: the origin wizard's replay store (handlers_origin_session
// /test opens it, /apply replays this instance's facts into it, /commit swaps
// it in) authors as the PERSISTED agent id, Deps.AgentID. The id differs from
// what the test fallback derives from "agent/test" ("test"), so a wizard that
// forgot SetAgentID authors the replay as "test" and is caught.
//
// SABOTAGE: drop remoteSvc.SetAgentID(rm.AgentID()) in
// handlers_origin_session.go → the replay commit is authored "test+…" → red.
func TestConnect_ReplayAuthorsAsThePersistedAgentID(t *testing.T) {
	const agentID = "opaque-web"
	if store.TestFallbackAgentID("agent/test") == agentID {
		t.Fatal("fixture: the id must differ from the fallback's")
	}
	home := t.TempDir()
	remotesRoot := filepath.Join(home, "remotes")
	if err := os.MkdirAll(remotesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(home, "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: remotesRoot},
		AgentBranch: "agent/test",
		AgentID:     agentID,
		KeyPath:     keyPath,
		Machine:     repos.Options{Synchronous: true},
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)
	s := &Server{Manager: m, SessionManager: sm, AgentBranch: "agent/test"}

	ri := createLocalRepo(t, m, "core")
	// A local fact for /apply to replay onto the remote's history.
	if err := ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), "agent/test", "kb/notes/local.md",
			"---\ntype: observation\nconfidence: 0.9\n---\n# local\n\nbody\n", "learn: local", "learn")
		if err != nil {
			t.Errorf("local write: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	url := seedKnomitRemoteForTest(t, filepath.Join(remotesRoot, "upstream.git"), "upstream")
	sessID := startOriginSession(t, s, "core", url)
	base := "/repos/core/origin-sessions/" + sessID
	if body := sseCall(t, s, http.MethodGet, base+"/test", ""); !strings.Contains(body, `"history":"disjoint"`) {
		t.Fatalf("fixture: want a disjoint remote; body=%s", body)
	}
	if body := sseCall(t, s, http.MethodPost, base+"/apply", `{"conflict_strategy":"local_wins"}`); strings.Contains(body, `"phase":"error"`) {
		t.Fatalf("apply: %s", body)
	}
	if body := sseCall(t, s, http.MethodPost, base+"/commit", ""); !strings.Contains(body, `"phase":"done"`) {
		t.Fatalf("commit: %s", body)
	}

	var email string
	if err := ri.WithRead(func(svc *store.Service) {
		ctx := context.Background()
		tip, err := svc.Branches().HeadCommit(ctx, "agent/test")
		if err != nil {
			t.Errorf("head: %v", err)
			return
		}
		info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(tip))
		if err != nil {
			t.Errorf("commit info: %v", err)
			return
		}
		email = info.AuthorEmail
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(email, agentID+"+") || !strings.HasSuffix(email, "@agents.knomit.io") {
		t.Fatalf("the replayed commit is authored %q, want %s+<op>@agents.knomit.io", email, agentID)
	}
}
