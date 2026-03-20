package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	"knomit/internal/git"
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

	localGS, err := git.InitWithStorer(localSvc.GitStorer(), nil, "agent/local")
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
	if _, _, err := remoteStore.WriteFile("kb/shared.md", sharedContent, "add shared", "learn"); err != nil {
		t.Fatalf("remote WriteFile shared: %v", err)
	}

	// Remote-only fact.
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Fact B\n\nOnly on remote.\n"
	if _, _, err := remoteStore.WriteFile("kb/remote-b.md", remoteFact, "add remote-b", "learn"); err != nil {
		t.Fatalf("remote WriteFile remote-b: %v", err)
	}

	// Advance main on remote so clone can find it.
	remoteHead, err := remoteStore.HeadCommit()
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

	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	ri := &RepoInstance{
		Name:       "knomit",
		GS:         localGS,
		Svc:        localSvc,
		Hub:        hub,
		SyncCancel: func() {},
		SyncWg:     &sync.WaitGroup{},
		StartSync: func(url string) error {
			return nil
		},
	}
	rm.Set("knomit", ri)
	handler := NewRouterWithSessionManager(rm, nil, false, "kb", sm)

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
	remote, err := updatedRI.Svc.GetRemote("origin")
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
