package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	"knomit/internal/git"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestOriginSession_FullWorkflow exercises the full remote connection workflow
// end-to-end: create session -> test connectivity -> preview -> apply ->
// apply again with different strategy -> commit.
func TestOriginSession_FullWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// ---- 1. Set up local knomit instance with facts ----

	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open local: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer local: %v", err)
	}

	// Write local-only facts.
	localFactA := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Fact A\n\nOnly on local.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/local-a.md", localFactA)

	// A shared fact (same path, same content on both local and remote).
	sharedContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Shared Fact\n\nPresent in both repos.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/shared.md", sharedContent)

	// ---- 2. Set up remote knomit instance with different facts ----
	// Sleep >1s so the remote's root commit has a different timestamp,
	// guaranteeing disjoint history (exercises the replay code path,
	// not the unimplemented shared-merge stub).
	time.Sleep(1100 * time.Millisecond)

	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}

	// Shared fact on remote (same path).
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/shared.md", sharedContent, "add shared", "learn"); err != nil {
		t.Fatalf("remote WriteFile shared: %v", err)
	}

	// Remote-only fact.
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Fact B\n\nOnly on remote.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/remote-b.md", remoteFact, "add remote-b", "learn"); err != nil {
		t.Fatalf("remote WriteFile remote-b: %v", err)
	}

	// Advance main on remote so clone can find it.
	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	// ---- 3. Wire in-process transport ----

	loader := server.MapLoader{"inmem:///integration-remote": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// ---- 4. Build router with local store ----

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// ---- Step 3: POST /session -> create session ----

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///integration-remote"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var createBody map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&createBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	sessionID := createBody["session_id"]
	if sessionID == "" {
		t.Fatal("expected non-empty session_id")
	}

	// ---- Step 4: GET /session/{id}/test -> clone and analyze (SSE) ----

	testReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, testReq)

	if testRec.Code != http.StatusOK {
		t.Fatalf("test connectivity: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}

	testEvents := parseSSEEvents(t, testRec.Body.String())
	assertHasPhase(t, testEvents, "connecting", "test connectivity")
	testDone := findDoneEvent(t, testEvents, "test connectivity")

	testResult, ok := testDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("test done event missing result: %v", testDone)
	}
	// Verify result has expected fields.
	if testResult["history"] == nil || testResult["history"] == "" {
		t.Error("test: expected non-empty history field")
	}
	if testResult["remote_fact_count"] == nil {
		t.Error("test: expected remote_fact_count")
	}
	if testResult["local_fact_count"] == nil {
		t.Error("test: expected local_fact_count")
	}

	// ---- Step 5: GET /session/{id}/preview -> compare (SSE) ----

	previewReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/preview", nil)
	previewRec := httptest.NewRecorder()
	handler.ServeHTTP(previewRec, previewReq)

	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", previewRec.Code, previewRec.Body.String())
	}

	previewEvents := parseSSEEvents(t, previewRec.Body.String())
	assertHasPhase(t, previewEvents, "comparing", "preview")
	previewDone := findDoneEvent(t, previewEvents, "preview")

	previewResult, ok := previewDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("preview done event missing result: %v", previewDone)
	}
	// local_only should be >= 1 (at least local-a.md).
	localOnly := int(previewResult["local_only"].(float64))
	if localOnly < 1 {
		t.Errorf("preview: expected local_only >= 1, got %d", localOnly)
	}
	// remote_only should be >= 1 (at least remote-b.md).
	remoteOnly := int(previewResult["remote_only"].(float64))
	if remoteOnly < 1 {
		t.Errorf("preview: expected remote_only >= 1, got %d", remoteOnly)
	}

	// ---- Step 6: POST /session/{id}/apply with local_wins (SSE) ----

	applyReq1 := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`))
	applyRec1 := httptest.NewRecorder()
	handler.ServeHTTP(applyRec1, applyReq1)

	if applyRec1.Code != http.StatusOK {
		t.Fatalf("apply (local_wins): expected 200, got %d: %s", applyRec1.Code, applyRec1.Body.String())
	}

	applyEvents1 := parseSSEEvents(t, applyRec1.Body.String())
	assertHasPhase(t, applyEvents1, "replaying", "apply local_wins")
	applyDone1 := findDoneEvent(t, applyEvents1, "apply local_wins")

	applyResult1, ok := applyDone1["result"].(map[string]any)
	if !ok {
		t.Fatalf("apply (local_wins) done event missing result: %v", applyDone1)
	}
	fromLocal1 := int(applyResult1["from_local"].(float64))
	fromRemote1 := int(applyResult1["from_remote"].(float64))
	totalFacts1 := int(applyResult1["total_facts"].(float64))
	if totalFacts1 < 2 {
		t.Errorf("apply (local_wins): expected total_facts >= 2, got %d", totalFacts1)
	}
	if fromLocal1 == 0 {
		t.Error("apply (local_wins): expected from_local > 0")
	}
	if fromRemote1 == 0 {
		t.Error("apply (local_wins): expected from_remote > 0")
	}

	// Verify session state is applied.
	sessRR := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if sessRR.Code != http.StatusOK {
		t.Fatalf("get session after apply: expected 200, got %d", sessRR.Code)
	}
	var sessBody map[string]any
	_ = json.NewDecoder(sessRR.Body).Decode(&sessBody)
	if sessBody["state"] != "applied" {
		t.Errorf("expected state=applied after first apply, got %v", sessBody["state"])
	}

	// ---- Step 7: POST /session/{id}/apply with remote_wins (SSE) ----

	applyReq2 := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"remote_wins"}`))
	applyRec2 := httptest.NewRecorder()
	handler.ServeHTTP(applyRec2, applyReq2)

	if applyRec2.Code != http.StatusOK {
		t.Fatalf("apply (remote_wins): expected 200, got %d: %s", applyRec2.Code, applyRec2.Body.String())
	}

	applyEvents2 := parseSSEEvents(t, applyRec2.Body.String())
	applyDone2 := findDoneEvent(t, applyEvents2, "apply remote_wins")
	applyResult2, ok := applyDone2["result"].(map[string]any)
	if !ok {
		t.Fatalf("apply (remote_wins) done event missing result: %v", applyDone2)
	}
	totalFacts2 := int(applyResult2["total_facts"].(float64))
	if totalFacts2 < 2 {
		t.Errorf("apply (remote_wins): expected total_facts >= 2, got %d", totalFacts2)
	}

	// ---- Step 8: POST /session/{id}/commit (SSE) ----

	commitReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/commit", nil)
	commitRec := httptest.NewRecorder()
	handler.ServeHTTP(commitRec, commitReq)

	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit: expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}

	commitEvents := parseSSEEvents(t, commitRec.Body.String())
	assertHasPhase(t, commitEvents, "swapping", "commit")
	assertHasPhase(t, commitEvents, "configuring", "commit")
	findDoneEvent(t, commitEvents, "commit")

	// Verify remote config saved to DB.
	updatedRI := rm.Get("knomit")
	if updatedRI == nil {
		t.Fatal("repo instance not found after commit")
	}
	var updatedSvc0 *store.Service
	updatedRI.WithRead(func(d repos.StoreDeps) { updatedSvc0 = d.Svc })
	remote, err := updatedSvc0.GetRemote("origin")
	if err != nil {
		t.Fatalf("GetRemote after commit: %v", err)
	}
	if remote == nil {
		t.Fatal("expected remote config to be saved after commit")
	}
	if remote.URL != "inmem:///integration-remote" {
		t.Errorf("expected remote URL=inmem:///integration-remote, got %q", remote.URL)
	}

	// ---- Step 9: GET /session/{id} -> 404 (session cleaned up) ----

	finalRR := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if finalRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after commit (session cleaned up), got %d: %s",
			finalRR.Code, finalRR.Body.String())
	}
}

// TestOriginSession_RemoteWinsStrategy verifies that remote_wins keeps remote
// content on shared paths and that commit saves the remote config.
func TestOriginSession_RemoteWinsStrategy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Local: 2 facts (local-a.md, shared.md).
	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open local: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer local: %v", err)
	}

	localOnlyFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Only\n\nLocal content.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/local-a.md", localOnlyFact)

	sharedLocalContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Shared Fact\n\nLocal version of shared.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/shared.md", sharedLocalContent)

	// Disjoint history: sleep so root commits differ.
	time.Sleep(1100 * time.Millisecond)

	// Remote: 3 facts (shared.md, remote-b.md, remote-c.md). 1 shared path = kb/shared.md.
	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}

	sharedRemoteContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Shared Fact\n\nRemote version of shared.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/shared.md", sharedRemoteContent, "add shared", "learn"); err != nil {
		t.Fatalf("remote WriteFile shared: %v", err)
	}
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote B\n\nRemote only.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/remote-b.md", remoteFact, "add remote-b", "learn"); err != nil {
		t.Fatalf("remote WriteFile remote-b: %v", err)
	}
	remoteFact2 := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote C\n\nRemote only.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/remote-c.md", remoteFact2, "add remote-c", "learn"); err != nil {
		t.Fatalf("remote WriteFile remote-c: %v", err)
	}

	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	loader := server.MapLoader{"inmem:///remote-wins-test": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Create session.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///remote-wins-test"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Test connectivity.
	testReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test connectivity: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, testRec.Body.String()), "test connectivity")

	// Apply with remote_wins.
	applyReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"remote_wins"}`))
	applyRec := httptest.NewRecorder()
	handler.ServeHTTP(applyRec, applyReq)
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply: expected 200, got %d: %s", applyRec.Code, applyRec.Body.String())
	}

	applyEvents := parseSSEEvents(t, applyRec.Body.String())
	applyDone := findDoneEvent(t, applyEvents, "apply remote_wins")
	result, ok := applyDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("apply done missing result: %v", applyDone)
	}

	// With remote_wins, the shared path (kb/shared.md) should keep remote content,
	// so from_local should NOT count the shared path. local-a.md is the only local-only fact.
	fromLocal := int(result["from_local"].(float64))
	if fromLocal != 1 {
		t.Errorf("expected from_local=1 (only local-a.md), got %d", fromLocal)
	}
	fromRemote := int(result["from_remote"].(float64))
	if fromRemote < 3 {
		t.Errorf("expected from_remote >= 3 (shared + remote-b + remote-c + kb.md), got %d", fromRemote)
	}

	// Commit and verify remote config saved.
	commitReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/commit", nil)
	commitRec := httptest.NewRecorder()
	handler.ServeHTTP(commitRec, commitReq)
	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit: expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, commitRec.Body.String()), "commit")

	updatedRI := rm.Get("knomit")
	var updatedSvc2 *store.Service
	updatedRI.WithRead(func(d repos.StoreDeps) { updatedSvc2 = d.Svc })
	remote, err := updatedSvc2.GetRemote("origin")
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote == nil {
		t.Fatal("expected remote config saved after commit")
	}
	if remote.URL != "inmem:///remote-wins-test" {
		t.Errorf("expected remote URL=inmem:///remote-wins-test, got %q", remote.URL)
	}
}

// TestOriginSession_SwitchStrategy verifies that apply can be called repeatedly
// with different strategies and the results differ for shared-path conflicts.
func TestOriginSession_SwitchStrategy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Local has 1 fact at a shared path.
	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	sharedLocal := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Shared\n\nLocal version.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/shared.md", sharedLocal)

	time.Sleep(1100 * time.Millisecond)

	// Remote has the same shared path with different content.
	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}
	sharedRemote := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Shared\n\nRemote version.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/shared.md", sharedRemote, "add shared", "learn"); err != nil {
		t.Fatalf("remote WriteFile: %v", err)
	}
	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	loader := server.MapLoader{"inmem:///switch-strategy": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Create session + test connectivity.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///switch-strategy"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	testReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}

	// Apply with local_wins.
	applyReq1 := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`))
	applyRec1 := httptest.NewRecorder()
	handler.ServeHTTP(applyRec1, applyReq1)
	if applyRec1.Code != http.StatusOK {
		t.Fatalf("apply local_wins: expected 200, got %d: %s", applyRec1.Code, applyRec1.Body.String())
	}
	done1 := findDoneEvent(t, parseSSEEvents(t, applyRec1.Body.String()), "apply local_wins")
	result1 := done1["result"].(map[string]any)
	fromLocal1 := int(result1["from_local"].(float64))

	// Apply again with remote_wins.
	applyReq2 := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"remote_wins"}`))
	applyRec2 := httptest.NewRecorder()
	handler.ServeHTTP(applyRec2, applyReq2)
	if applyRec2.Code != http.StatusOK {
		t.Fatalf("apply remote_wins: expected 200, got %d: %s", applyRec2.Code, applyRec2.Body.String())
	}
	done2 := findDoneEvent(t, parseSSEEvents(t, applyRec2.Body.String()), "apply remote_wins")
	result2 := done2["result"].(map[string]any)
	fromLocal2 := int(result2["from_local"].(float64))

	// With local_wins the shared path counts as from_local; with remote_wins it does not.
	if fromLocal1 <= fromLocal2 {
		t.Errorf("expected from_local with local_wins (%d) > from_local with remote_wins (%d)",
			fromLocal1, fromLocal2)
	}
}

// TestOriginSession_ExistingAgentBranch verifies that when the remote already
// has an agent branch matching the local branch name, the test step reports
// matched_agent and apply replays on top of the existing branch.
func TestOriginSession_ExistingAgentBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Local store with a fact.
	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	localFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Fact\n\nLocal content.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/local.md", localFact)

	time.Sleep(1100 * time.Millisecond)

	// Remote store with an agent branch matching the local agent branch.
	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}

	// Write a fact on the agent branch so it has content.
	existingFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Existing Remote Fact\n\nAlready on agent branch.\n"
	if _, _, err := remoteStore.WriteFile(testAgentBranch, "kb/existing-remote.md", existingFact, "add existing", "learn"); err != nil {
		t.Fatalf("remote WriteFile existing: %v", err)
	}

	// Set up main branch on remote.
	remoteHead, err := remoteStore.HeadCommit(testAgentBranch)
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	loader := server.MapLoader{"inmem:///existing-agent": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Create session.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///existing-agent"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Test connectivity — should report matched_agent.
	testReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}

	testEvents := parseSSEEvents(t, testRec.Body.String())
	testDone := findDoneEvent(t, testEvents, "test connectivity")
	testResult, ok := testDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("test done missing result: %v", testDone)
	}
	matchedAgent, _ := testResult["matched_agent"].(string)
	if matchedAgent != testAgentBranch {
		t.Errorf("expected matched_agent=%q, got %q", testAgentBranch, matchedAgent)
	}

	// Apply with local_wins — should replay on top of existing agent branch.
	applyReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`))
	applyRec := httptest.NewRecorder()
	handler.ServeHTTP(applyRec, applyReq)
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply: expected 200, got %d: %s", applyRec.Code, applyRec.Body.String())
	}

	applyDone := findDoneEvent(t, parseSSEEvents(t, applyRec.Body.String()), "apply")
	applyRes, ok := applyDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("apply done missing result: %v", applyDone)
	}

	// Total should include both the existing remote fact and the replayed local fact.
	totalFacts := int(applyRes["total_facts"].(float64))
	if totalFacts < 2 {
		t.Errorf("expected total_facts >= 2 (existing + local), got %d", totalFacts)
	}
	fromRemote := int(applyRes["from_remote"].(float64))
	if fromRemote == 0 {
		t.Error("expected from_remote > 0 (existing facts on agent branch)")
	}
}

// TestOriginSession_CancelCleanup verifies that deleting a session cleans up
// its temp directory and makes the session return 404.
func TestOriginSession_CancelCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Create session.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///cancel-test"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Retrieve the session's temp dir before deleting.
	sess, ok := sm.Get("knomit", sessionID)
	if !ok {
		t.Fatal("session not found after create")
	}
	tempDir := sess.TempDir
	if tempDir == "" {
		t.Fatal("expected non-empty TempDir")
	}

	// Verify temp dir exists.
	if _, err := os.Stat(tempDir); os.IsNotExist(err) {
		t.Fatal("expected temp dir to exist before delete")
	}

	// Delete session (cancel).
	delRR := doRequest(t, handler, http.MethodDelete, "/api/v1/knomit/origin/session/"+sessionID, "")
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", delRR.Code)
	}

	// Verify temp dir is gone.
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Errorf("expected temp dir to be removed after delete, but it still exists")
	}

	// Verify GET returns 404.
	getRR := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if getRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d: %s", getRR.Code, getRR.Body.String())
	}
}

// TestOriginSession_BranchSelection verifies that branches are listed in the
// test result and that applying with an explicit branch saves that branch in
// the remote config. The remote's HEAD is pointed at "main" so the clone
// checks it out as refs/heads/main, making it available for branch selection.
func TestOriginSession_BranchSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	localFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Fact\n\nContent.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/local.md", localFact)

	time.Sleep(1100 * time.Millisecond)

	// Remote with "main" and "develop" branches. HEAD set to "main" so the
	// clone checks out refs/heads/main (making it available in branches list).
	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Fact\n\nContent.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/remote.md", remoteFact, "add remote", "learn"); err != nil {
		t.Fatalf("remote WriteFile: %v", err)
	}

	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	// Create both branches.
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("develop"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference develop: %v", err)
	}
	// Point HEAD to main so clone checks out refs/heads/main.
	if err := remoteStorer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")),
	); err != nil {
		t.Fatalf("remote SetReference HEAD: %v", err)
	}

	loader := server.MapLoader{"inmem:///branch-selection": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Create session.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///branch-selection"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Test connectivity — check branches list contains "main".
	testReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}

	testEvents := parseSSEEvents(t, testRec.Body.String())
	testDone := findDoneEvent(t, testEvents, "test connectivity")
	testResult, ok := testDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("test done missing result: %v", testDone)
	}

	branchesRaw, _ := testResult["branches"].([]any)
	branchNames := make(map[string]bool)
	for _, b := range branchesRaw {
		branchNames[b.(string)] = true
	}
	if !branchNames["main"] {
		t.Errorf("expected branches to contain 'main', got %v", branchesRaw)
	}

	defaultBranch, _ := testResult["default_branch"].(string)
	if defaultBranch != "main" {
		t.Errorf("expected default_branch=main, got %q", defaultBranch)
	}

	// Apply with explicit branch="main".
	applyReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins","branch":"main"}`))
	applyRec := httptest.NewRecorder()
	handler.ServeHTTP(applyRec, applyReq)
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply: expected 200, got %d: %s", applyRec.Code, applyRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, applyRec.Body.String()), "apply")

	// Commit.
	commitReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/commit", nil)
	commitRec := httptest.NewRecorder()
	handler.ServeHTTP(commitRec, commitReq)
	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit: expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, commitRec.Body.String()), "commit")

	// Verify remote config saved with branch="main".
	updatedRI := rm.Get("knomit")
	var updatedSvc3 *store.Service
	updatedRI.WithRead(func(d repos.StoreDeps) { updatedSvc3 = d.Svc })
	remote, err := updatedSvc3.GetRemote("origin")
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote == nil {
		t.Fatal("expected remote config saved after commit")
	}
	if remote.Branch != "main" {
		t.Errorf("expected remote branch=main, got %q", remote.Branch)
	}
}

// TestOriginSession_RebuildAfterCommit verifies that after the full workflow
// through commit, the index has been rebuilt and RecentFacts returns data.
func TestOriginSession_RebuildAfterCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	localFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Fact\n\nContent.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/local.md", localFact)

	time.Sleep(1100 * time.Millisecond)

	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Fact\n\nDifferent content.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/remote.md", remoteFact, "add remote", "learn"); err != nil {
		t.Fatalf("remote WriteFile: %v", err)
	}
	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	loader := server.MapLoader{"inmem:///rebuild-test": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Create session.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///rebuild-test"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Test connectivity.
	testReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}

	// Apply with local_wins.
	applyReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`))
	applyRec := httptest.NewRecorder()
	handler.ServeHTTP(applyRec, applyReq)
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply: expected 200, got %d: %s", applyRec.Code, applyRec.Body.String())
	}

	// Commit.
	commitReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/commit", nil)
	commitRec := httptest.NewRecorder()
	handler.ServeHTTP(commitRec, commitReq)
	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit: expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, commitRec.Body.String()), "commit")

	// Verify index was rebuilt: RecentFacts should return data.
	updatedRI := rm.Get("knomit")
	if updatedRI == nil {
		t.Fatal("repo instance not found after commit")
	}
	var updatedSvc4 *store.Service
	updatedRI.WithRead(func(d repos.StoreDeps) { updatedSvc4 = d.Svc })
	facts, total, err := updatedSvc4.Index().RecentFacts("", "", 100, 0, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RecentFacts: %v", err)
	}
	if total == 0 || len(facts) == 0 {
		t.Errorf("expected RecentFacts to return data after rebuild, got total=%d len=%d", total, len(facts))
	}
}

// TestOriginSession_ReviewWatermarkSetAfterCommit verifies that after the
// origin session commit (clone + rebuild), the review watermark is set to HEAD
// so the first review doesn't treat every cloned fact as dirty.
func TestOriginSession_ReviewWatermarkSetAfterCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	// Write a fact so the remote has content.
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Cloned Fact\n\nThis was cloned.\n"

	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/cloned.md", remoteFact, "add fact", "learn"); err != nil {
		t.Fatalf("remote WriteFile: %v", err)
	}
	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	loader := server.MapLoader{"inmem:///watermark-test": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Run the full workflow: create -> test -> apply -> commit.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///watermark-test"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil))

	applyRec := httptest.NewRecorder()
	handler.ServeHTTP(applyRec, httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`)))

	commitRec := httptest.NewRecorder()
	handler.ServeHTTP(commitRec, httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/commit", nil))
	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit: expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, commitRec.Body.String()), "commit")

	// All pipeline watermarks should be set to HEAD after rebuild.
	// After a shared-history commit, the swapped store is the clone. The clone's
	// HEAD is the remote's agent branch ("agent/remote" in this test), which is
	// what handleCommit uses as rebuildBranch (first entry in AgentBranches).
	updatedRI := rm.Get("knomit")
	var updatedSvc5 *store.Service
	var updatedGS repos.GitStore
	updatedRI.WithRead(func(d repos.StoreDeps) {
		updatedSvc5 = d.Svc
		updatedGS = d.GS
	})
	idx := updatedSvc5.Index()
	// The remote was initialized with "agent/remote" as its agent branch.
	rebuildBranch := "agent/remote"

	head, err := updatedGS.HeadCommit(rebuildBranch)
	if err != nil {
		t.Fatalf("HeadCommit(%s): %v", rebuildBranch, err)
	}

	for _, tool := range []string{"review", "hypothesize"} {
		watermark, err := idx.GetPipelineWatermark(tool, rebuildBranch)
		if err != nil {
			t.Fatalf("GetPipelineWatermark(%s, %s): %v", tool, rebuildBranch, err)
		}
		if watermark == "" {
			t.Fatalf("%s watermark should be set after origin session commit, but was empty", tool)
		}
		if watermark != head {
			t.Errorf("%s watermark = %q, want HEAD = %q", tool, watermark, head)
		}
	}
}

// TestOriginSession_DeadRefs verifies that preview reports dead_refs_found > 0
// when a local fact references a path that does not exist, and 0 when all refs
// point to existing facts.
func TestOriginSession_DeadRefs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// ---- Local store: one fact with a dead ref, one valid fact ----
	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	// A plain fact with no refs (dead_refs = 0 contribution).
	validFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Valid Fact\n\nNo refs.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/valid.md", validFact)

	// A fact whose refs list contains a path that does not exist in the local store.
	deadRefFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs:\n  - kb/nonexistent.md\n---\n# Fact With Dead Ref\n\nPoints to a missing file.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/with-dead-ref.md", deadRefFact)

	// ---- Remote store: minimal, just needs to be clonable ----
	time.Sleep(1100 * time.Millisecond)

	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Fact\n\nContent.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/remote.md", remoteFact, "add remote", "learn"); err != nil {
		t.Fatalf("remote WriteFile: %v", err)
	}
	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	loader := server.MapLoader{"inmem:///dead-refs-test": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	// Create session.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///dead-refs-test"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Test connectivity.
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil))
	if testRec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, testRec.Body.String()), "test connectivity")

	// Preview — should report dead_refs_found >= 1.
	previewRec := httptest.NewRecorder()
	handler.ServeHTTP(previewRec, httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/preview", nil))
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", previewRec.Code, previewRec.Body.String())
	}

	previewDone := findDoneEvent(t, parseSSEEvents(t, previewRec.Body.String()), "preview")
	previewResult, ok := previewDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("preview done missing result: %v", previewDone)
	}

	deadRefsFound := int(previewResult["dead_refs_found"].(float64))
	if deadRefsFound < 1 {
		t.Errorf("expected dead_refs_found >= 1 (kb/nonexistent.md is missing), got %d", deadRefsFound)
	}

	// Also verify the valid fact does not inflate the dead-refs count beyond what we expect.
	// There is exactly 1 dead ref (kb/nonexistent.md from kb/with-dead-ref.md).
	if deadRefsFound != 1 {
		t.Errorf("expected dead_refs_found == 1, got %d", deadRefsFound)
	}
}

// TestOriginSession_NoDeadRefs verifies that preview reports dead_refs_found == 0
// when all refs in local facts point to paths that exist.
func TestOriginSession_NoDeadRefs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	localSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { localSvc.Close() })

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, testAgentBranch)
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	// Two facts; one references the other (valid ref).
	factA := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Fact A\n\nContent.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/fact-a.md", factA)

	// This fact refs fact-a.md which exists.
	factB := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs:\n  - kb/fact-a.md\n---\n# Fact B\n\nReferences fact-a.\n"
	writeFact(t, localGS, localSvc.DB(), "kb/fact-b.md", factB)

	time.Sleep(1100 * time.Millisecond)

	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatalf("git.InitWithStorer remote: %v", err)
	}
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Fact\n\nContent.\n"
	if _, _, err := remoteStore.WriteFile("agent/remote", "kb/remote.md", remoteFact, "add remote", "learn"); err != nil {
		t.Fatalf("remote WriteFile: %v", err)
	}
	remoteHead, err := remoteStore.HeadCommit("agent/remote")
	if err != nil {
		t.Fatalf("remote HeadCommit: %v", err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatalf("remote SetReference main: %v", err)
	}

	loader := server.MapLoader{"inmem:///no-dead-refs-test": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		AgentBranch: testAgentBranch,
		Name:        "knomit",
		GS:          localGS,
		Svc:         localSvc,
		Hub:         hub,
		StartSync:   func(url string) error { return nil },
	})
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", testAgentBranch, sm)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///no-dead-refs-test"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/test", nil))
	if testRec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}
	findDoneEvent(t, parseSSEEvents(t, testRec.Body.String()), "test connectivity")

	previewRec := httptest.NewRecorder()
	handler.ServeHTTP(previewRec, httptest.NewRequest(http.MethodGet,
		"/api/v1/knomit/origin/session/"+sessionID+"/preview", nil))
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", previewRec.Code, previewRec.Body.String())
	}

	previewDone := findDoneEvent(t, parseSSEEvents(t, previewRec.Body.String()), "preview")
	previewResult, ok := previewDone["result"].(map[string]any)
	if !ok {
		t.Fatalf("preview done missing result: %v", previewDone)
	}

	deadRefsFound := int(previewResult["dead_refs_found"].(float64))
	if deadRefsFound != 0 {
		t.Errorf("expected dead_refs_found == 0 (all refs are valid), got %d", deadRefsFound)
	}
}

// assertHasPhase checks that at least one SSE event contains the given phase.
func assertHasPhase(t *testing.T, events []map[string]any, phase, step string) {
	t.Helper()
	for _, ev := range events {
		if ev["phase"] == phase {
			return
		}
	}
	t.Errorf("%s: expected phase %q in events, got: %v", step, phase, events)
}

// findDoneEvent returns the first SSE event with phase=done, or fails the test.
func findDoneEvent(t *testing.T, events []map[string]any, step string) map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev["phase"] == "done" {
			return ev
		}
	}
	t.Fatalf("%s: no done event found in: %v", step, events)
	return nil
}
