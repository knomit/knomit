package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// TestConnect_DisjointReplaySignsWithTheInstanceKey: on the disjoint-history
// path the wizard REPLAYS this instance's facts into the clone store opened by
// /test, SwapStore installs that store, and its agent branch is pushed. That
// store had no signer, so every replayed commit went out unsigned (F09 PR 2).
// Each commit the replay adds on top of the remote's main must now carry an
// SSH signature by the INSTANCE key (Deps.Signer), never the test fallback.
func TestConnect_DisjointReplaySignsWithTheInstanceKey(t *testing.T) {
	home := t.TempDir()
	remotesRoot := filepath.Join(home, "remotes")
	if err := os.MkdirAll(remotesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(home, "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	instance := testsigner.Named("wizard-instance")
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: remotesRoot},
		AgentBranch: "agent/test",
		KeyPath:     keyPath,
		Signer:      instance,
		Machine:     repos.Options{Synchronous: true},
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)
	s := &Server{Manager: m, SessionManager: sm, AgentBranch: "agent/test"}

	local := createLocalRepo(t, m, "core")
	// A local fact, so the disjoint replay has something to copy.
	var werr error
	if err := local.WithRead(func(c *store.Service) {
		_, werr = c.Facts().WriteFact(context.Background(), "agent/test", "kb/notes/local.md",
			"---\ntype: observation\nconfidence: 0.9\ndomain: [test]\n---\n# local\n\nlocal fact\n", "learn: local", "created")
	}); err != nil {
		t.Fatal(err)
	}
	if werr != nil {
		t.Fatalf("seed local fact: %v", werr)
	}
	bare := filepath.Join(remotesRoot, "upstream.git")
	url := seedKnomitRemoteForTest(t, bare, "upstream")

	sessID := startOriginSession(t, s, "core", url)
	base := "/repos/core/origin-sessions/" + sessID
	if body := sseCall(t, s, http.MethodGet, base+"/test", ""); !strings.Contains(body, `"history":"disjoint"`) {
		t.Fatalf("expected a disjoint-history test result; body=%s", body)
	}
	if body := sseCall(t, s, http.MethodPost, base+"/apply", `{"conflict_strategy":"local_wins"}`); !strings.Contains(body, `"from_local":1`) {
		t.Fatalf("apply must replay the one local fact; body=%s", body)
	}
	if body := sseCall(t, s, http.MethodPost, base+"/commit", ""); !strings.Contains(body, `"phase":"done"`) {
		t.Fatalf("commit did not complete; body=%s", body)
	}

	// Push the swapped-in agent branch to the bare remote and read it with git.
	ri := m.Get("core")
	if ri == nil {
		t.Fatal("repo core missing after commit")
	}
	var perr error
	if err := ri.WithRead(func(c *store.Service) {
		_, perr = c.Remote().Push(context.Background(), "agent/test", nil)
	}); err != nil {
		t.Fatal(err)
	}
	if perr != nil {
		t.Fatalf("push agent branch: %v", perr)
	}

	revs := strings.Fields(gitOutputForTest(t, bare, "rev-list", "refs/heads/agent/test", "^refs/heads/main"))
	if len(revs) == 0 {
		t.Fatal("the replay added no commits on top of the remote's main; the fixture proves nothing")
	}
	for _, rev := range revs {
		key, ok, err := testsigner.CommitSignerKey(bare, rev)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("replayed commit %s is UNSIGNED", rev)
		}
		if !testsigner.SameKey(key, instance.PublicKey()) {
			t.Fatalf("replayed commit %s is not signed by the instance key", rev)
		}
	}
}
