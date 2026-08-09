package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// The origin-connect session is the ONE flow that can change a repo's
// identity: the disjoint-history branch of /commit swaps the repo's whole
// store for the temp clone, after which its root commit — and therefore the
// knowledge base it holds — is the remote's. These tests pin both halves of
// that contract: the registry must FOLLOW a legitimate adopt, and a connect
// that would produce a second local copy of an already-registered knowledge
// base must be REFUSED before any of it happens.

// seedKnomitRemoteForTest builds a bare git repo at bare holding a single fact
// under kb/ on "main" and returns its file:// URL — the shape /test clones and
// /apply replays into. Each call produces an INDEPENDENT root commit (the
// commit timestamp and content differ per call), which is what makes two
// remotes seeded this way genuinely disjoint knowledge bases.
func seedKnomitRemoteForTest(t *testing.T, bare, factBody string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	if err := os.MkdirAll(filepath.Join(work, "kb"), 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	fact := "---\ntitle: seed\n---\n\n" + factBody + "\n"
	if err := os.WriteFile(filepath.Join(work, "kb", "seed.md"), []byte(fact), 0o644); err != nil {
		t.Fatalf("write seed fact: %v", err)
	}
	runGitForTest(t, work, "add", "kb/seed.md")
	runGitForTest(t, work, "commit", "-m", "seed "+factBody)
	runGitForTest(t, work, "push", "origin", "main")
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return "file://" + bare
}

// rootCommitOfBare returns the root commit hash of branch in a bare repo,
// read with real git so the expectation is independent of the store code
// under test.
func rootCommitOfBare(t *testing.T, bare, branch string) string {
	t.Helper()
	out := gitOutputForTest(t, bare, "rev-list", "--max-parents=0", branch)
	fields := strings.Fields(out)
	if len(fields) != 1 {
		t.Fatalf("expected exactly one root commit in %s, got %v", bare, fields)
	}
	return fields[0]
}

// gitOutputForTest is runGitForTest's read-only twin: same command shape, but
// stdout is returned instead of discarded.
func gitOutputForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return string(out)
}

// newIdentitySessionServer boots a REAL Manager (control.db opened by Start,
// repos created through the production Create path) plus a SessionManager, and
// returns the server together with the directory bare remotes must be seeded
// under — it is the manager's LocalOriginRoot, so file:// origins beneath it
// clear the local-origin policy the clone boundary enforces.
func newIdentitySessionServer(t *testing.T) (*Server, *repos.Manager, string) {
	t.Helper()
	home := t.TempDir()
	remotesRoot := filepath.Join(home, "remotes")
	if err := os.MkdirAll(remotesRoot, 0o755); err != nil {
		t.Fatalf("mkdir remotes: %v", err)
	}
	keyPath := filepath.Join(home, "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{
			Home:            home,
			OntologyRoot:    "kb",
			LocalOriginRoot: remotesRoot,
		},
		AgentBranch:           "agent/test",
		KeyPath:               keyPath,
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}

	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	return &Server{Manager: m, SessionManager: sm, AgentBranch: "agent/test"}, m, remotesRoot
}

func createLocalRepo(t *testing.T, m *repos.Manager, name string) *repos.RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: name, Mode: "preset"}, nil)
	if err != nil {
		t.Fatalf("create repo %q: %v", name, err)
	}
	return ri
}

// startOriginSession POSTs /origin-sessions and returns the new session id.
func startOriginSession(t *testing.T, s *Server, repo, url string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/"+repo+"/origin-sessions",
		strings.NewReader(`{"url":`+mustJSON(t, url)+`}`))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create session: status %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode session response: %v (body=%s)", err, rec.Body.String())
	}
	if body.SessionID == "" {
		t.Fatalf("no session_id in response: %s", rec.Body.String())
	}
	return body.SessionID
}

// sseCall drives one SSE step of the connect session and returns the raw
// stream body.
func sseCall(t *testing.T, s *Server, method, path, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	s.NewAPIRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: status %d, body=%s", method, path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestConnect_DivergentAdoptUpdatesIdentity runs the full four-step connect
// session — POST /origin-sessions, /test, /apply, /commit — against a remote
// with history disjoint from the local repo's. That is an ADOPT: the swap at
// /commit makes the remote's knowledge base the repo's own, so the registry's
// repo_id must follow it. The serving profile is keyed by uid, not by
// identity, so it must survive the adopt untouched.
func TestConnect_DivergentAdoptUpdatesIdentity(t *testing.T) {
	s, m, remotesRoot := newIdentitySessionServer(t)

	ri := createLocalRepo(t, m, "core")
	uid := ri.UID()
	if err := m.Repos().SetProfile(uid, repos.ProfileChat); err != nil {
		t.Fatalf("set profile: %v", err)
	}

	before, ok, err := m.Repos().Get(uid)
	if err != nil || !ok {
		t.Fatalf("registry get: %v (ok=%v)", err, ok)
	}
	if before.RepoID == "" {
		t.Fatal("locally created repo has no recorded repo_id")
	}

	bare := filepath.Join(remotesRoot, "upstream.git")
	url := seedKnomitRemoteForTest(t, bare, "upstream")
	remoteRoot := rootCommitOfBare(t, bare, "main")
	if remoteRoot == before.RepoID {
		t.Fatal("fixture is not disjoint: remote and local share a root commit")
	}

	sessID := startOriginSession(t, s, "core", url)
	base := "/repos/core/origin-sessions/" + sessID

	testBody := sseCall(t, s, http.MethodGet, base+"/test", "")
	if !strings.Contains(testBody, `"history":"disjoint"`) {
		t.Fatalf("expected a disjoint-history test result; body=%s", testBody)
	}
	if strings.Contains(testBody, `"phase":"error"`) {
		t.Fatalf("test step errored; body=%s", testBody)
	}

	applyBody := sseCall(t, s, http.MethodPost, base+"/apply", `{"conflict_strategy":"local_wins"}`)
	if strings.Contains(applyBody, `"phase":"error"`) {
		t.Fatalf("apply step errored; body=%s", applyBody)
	}

	commitBody := sseCall(t, s, http.MethodPost, base+"/commit", "")
	if strings.Contains(commitBody, `"phase":"error"`) {
		t.Fatalf("commit step errored; body=%s", commitBody)
	}
	if !strings.Contains(commitBody, `"phase":"done"`) {
		t.Fatalf("commit did not complete; body=%s", commitBody)
	}

	after, ok, err := m.Repos().Get(uid)
	if err != nil || !ok {
		t.Fatalf("registry get after commit: %v (ok=%v)", err, ok)
	}
	if after.RepoID != remoteRoot {
		t.Errorf("repo_id after adopt: got %q, want the remote's root %q", after.RepoID, remoteRoot)
	}
	if after.Profile != repos.ProfileChat {
		t.Errorf("profile after adopt: got %q, want %q — the profile is keyed by uid and must survive an identity change",
			after.Profile, repos.ProfileChat)
	}

	// The connection the session negotiated lands in control.db, not in the
	// repo's own remotes row: the .db that was just swapped wholesale is
	// exactly the file control.db has to outlive.
	org, err := m.Origins().Get(uid)
	if err != nil {
		t.Fatalf("origins get: %v", err)
	}
	if org == nil {
		t.Fatal("commit did not persist the origin to control.db")
	}
	if org.URL != url {
		t.Errorf("control.db origin url: got %q, want %q", org.URL, url)
	}
	if org.Branch != "main" {
		t.Errorf("control.db origin branch: got %q, want %q", org.Branch, "main")
	}
}

// TestConnect_AlreadyRegisteredRemoteRejectedAtTest pins the constraint the
// registry exists for: one local copy per knowledge base. "alpha" already
// holds remote R's knowledge base, so connecting "beta" to R would give this
// machine two repos writing the same agent/<host> branch, clobbering each
// other on push. The refusal must land at /test — the FIRST step of the wizard
// — so it happens before any preview or replay work, and long before the swap,
// which cannot be undone.
func TestConnect_AlreadyRegisteredRemoteRejectedAtTest(t *testing.T) {
	s, m, remotesRoot := newIdentitySessionServer(t)

	bare := filepath.Join(remotesRoot, "shared.git")
	url := seedKnomitRemoteForTest(t, bare, "shared")

	alpha, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "alpha", Mode: "clone", Origin: &repos.OriginSpec{URL: url},
	}, nil)
	if err != nil {
		t.Fatalf("clone alpha from the shared remote: %v", err)
	}
	alphaRec, ok, err := m.Repos().Get(alpha.UID())
	if err != nil || !ok {
		t.Fatalf("registry get alpha: %v (ok=%v)", err, ok)
	}
	if alphaRec.RepoID != rootCommitOfBare(t, bare, "main") {
		t.Fatalf("alpha did not adopt the remote's identity: %q", alphaRec.RepoID)
	}

	beta := createLocalRepo(t, m, "beta")
	betaBefore, _, err := m.Repos().Get(beta.UID())
	if err != nil {
		t.Fatalf("registry get beta: %v", err)
	}

	sessID := startOriginSession(t, s, "beta", url)
	body := sseCall(t, s, http.MethodGet, "/repos/beta/origin-sessions/"+sessID+"/test", "")

	if !strings.Contains(body, `"phase":"error"`) {
		t.Fatalf("connecting beta to a remote alpha already holds must fail at /test; body=%s", body)
	}
	if !strings.Contains(body, "alpha") {
		t.Errorf("the refusal must name the repo already holding the knowledge base; body=%s", body)
	}
	if strings.Contains(body, `"phase":"done"`) {
		t.Errorf("/test must not report success after refusing; body=%s", body)
	}

	// Nothing was applied: the session never reached a state /apply or
	// /commit will act on, and beta still holds its own knowledge base.
	sess, ok := s.SessionManager.Get("beta", sessID)
	if !ok {
		t.Fatal("session vanished")
	}
	sess.mu.Lock()
	state := sess.State
	sess.mu.Unlock()
	if state == StateTested || state == StatePreviewed || state == StateApplied {
		t.Errorf("session state after a refused /test: got %q, want a state /apply refuses", state)
	}

	betaAfter, _, err := m.Repos().Get(beta.UID())
	if err != nil {
		t.Fatalf("registry get beta after: %v", err)
	}
	if betaAfter.RepoID != betaBefore.RepoID {
		t.Errorf("beta's identity changed: %q → %q", betaBefore.RepoID, betaAfter.RepoID)
	}

	// /apply must refuse outright — the refusal at /test is not advisory.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/beta/origin-sessions/"+sessID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("/apply after a refused /test: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestConnect_RemoteClaimedBetweenTestAndCommitRejectedBeforeSwap covers the
// window the /test check cannot: /test passed, /apply replayed, and only THEN
// did another repo register the same knowledge base. The re-check sits
// immediately before rm.SwapStore because the swap is the point of no return —
// after it the local store already IS the merged result and nothing can undo
// it. Refusing here must leave the repo exactly as it was.
func TestConnect_RemoteClaimedBetweenTestAndCommitRejectedBeforeSwap(t *testing.T) {
	s, m, remotesRoot := newIdentitySessionServer(t)

	bare := filepath.Join(remotesRoot, "contested.git")
	url := seedKnomitRemoteForTest(t, bare, "contested")

	beta := createLocalRepo(t, m, "beta")
	betaBefore, _, err := m.Repos().Get(beta.UID())
	if err != nil {
		t.Fatalf("registry get beta: %v", err)
	}

	sessID := startOriginSession(t, s, "beta", url)
	base := "/repos/beta/origin-sessions/" + sessID

	testBody := sseCall(t, s, http.MethodGet, base+"/test", "")
	if strings.Contains(testBody, `"phase":"error"`) {
		t.Fatalf("test step errored; body=%s", testBody)
	}
	applyBody := sseCall(t, s, http.MethodPost, base+"/apply", `{"conflict_strategy":"local_wins"}`)
	if strings.Contains(applyBody, `"phase":"error"`) {
		t.Fatalf("apply step errored; body=%s", applyBody)
	}

	// The race: alpha claims the knowledge base after beta's session was
	// tested and applied, but before beta commits.
	if _, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "alpha", Mode: "clone", Origin: &repos.OriginSpec{URL: url},
	}, nil); err != nil {
		t.Fatalf("clone alpha from the contested remote: %v", err)
	}

	commitBody := sseCall(t, s, http.MethodPost, base+"/commit", "")
	if !strings.Contains(commitBody, `"phase":"error"`) {
		t.Fatalf("commit must refuse a swap that duplicates a registered knowledge base; body=%s", commitBody)
	}
	if !strings.Contains(commitBody, "alpha") {
		t.Errorf("the refusal must name the repo already holding the knowledge base; body=%s", commitBody)
	}
	if strings.Contains(commitBody, `"phase":"done"`) {
		t.Errorf("commit must not report success after refusing; body=%s", commitBody)
	}

	betaAfter, _, err := m.Repos().Get(beta.UID())
	if err != nil {
		t.Fatalf("registry get beta after: %v", err)
	}
	if betaAfter.RepoID != betaBefore.RepoID {
		t.Errorf("the swap happened anyway: beta's identity moved %q → %q", betaBefore.RepoID, betaAfter.RepoID)
	}
	if beta.ID() != betaBefore.RepoID {
		t.Errorf("beta's live store was swapped: root commit is now %q, want %q", beta.ID(), betaBefore.RepoID)
	}
}

// TestConnect_ReconnectToOwnRemoteNotRefused pins the exclusion that keeps the
// guard from being a foot-gun: a repo re-testing the connection to the remote
// it ALREADY holds is not a duplicate. Without the selfUID exclusion the check
// would match the repo's own registry row and make every reconnect impossible.
func TestConnect_ReconnectToOwnRemoteNotRefused(t *testing.T) {
	s, m, remotesRoot := newIdentitySessionServer(t)

	bare := filepath.Join(remotesRoot, "own.git")
	url := seedKnomitRemoteForTest(t, bare, "own")

	alpha, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "alpha", Mode: "clone", Origin: &repos.OriginSpec{URL: url},
	}, nil)
	if err != nil {
		t.Fatalf("clone alpha: %v", err)
	}
	rec, ok, err := m.Repos().Get(alpha.UID())
	if err != nil || !ok {
		t.Fatalf("registry get alpha: %v (ok=%v)", err, ok)
	}
	if rec.RepoID != rootCommitOfBare(t, bare, "main") {
		t.Fatalf("alpha does not hold the remote's knowledge base: %q", rec.RepoID)
	}

	sessID := startOriginSession(t, s, "alpha", url)
	body := sseCall(t, s, http.MethodGet, "/repos/alpha/origin-sessions/"+sessID+"/test", "")

	if strings.Contains(body, `"phase":"error"`) {
		t.Fatalf("a repo must not collide with itself; body=%s", body)
	}
	if !strings.Contains(body, `"phase":"done"`) {
		t.Fatalf("/test did not complete; body=%s", body)
	}
}
