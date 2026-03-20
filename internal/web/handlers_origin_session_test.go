package web

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/git"
	"knomit/internal/store"
	storegit "knomit/internal/store/git"
)

func TestCreateSession_ValidInput(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["session_id"] == "" {
		t.Error("expected non-empty session_id")
	}
}

func TestCreateSession_InvalidURL(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"not-a-url","auth_method":"token"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateSession_HTTPWithSSHAuth(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"ssh"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["error"] == "" {
		t.Error("expected error message")
	}
}

func TestCreateSession_ReplacesExisting(t *testing.T) {
	handler := newTestRouter(nil, nil)

	// First session
	rr1 := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first create: expected 200, got %d: %s", rr1.Code, rr1.Body.String())
	}
	var body1 map[string]string
	_ = json.NewDecoder(rr1.Body).Decode(&body1)
	firstID := body1["session_id"]

	// Second session (replaces first)
	rr2 := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo2.git","auth_method":"token","token":"ghp_def"}`)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second create: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
	var body2 map[string]string
	_ = json.NewDecoder(rr2.Body).Decode(&body2)
	secondID := body2["session_id"]

	if firstID == secondID {
		t.Error("expected different session IDs")
	}

	// First session should be gone (get returns 404)
	rr3 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+firstID, "")
	if rr3.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for replaced session, got %d", rr3.Code)
	}

	// Second session should be accessible
	rr4 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+secondID, "")
	if rr4.Code != http.StatusOK {
		t.Fatalf("expected 200 for current session, got %d: %s", rr4.Code, rr4.Body.String())
	}
}

func TestGetSession_Found(t *testing.T) {
	handler := newTestRouter(nil, nil)

	// Create
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d", rr.Code)
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Get
	rr2 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}

	var getBody map[string]any
	if err := json.NewDecoder(rr2.Body).Decode(&getBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if getBody["session_id"] != sessionID {
		t.Errorf("expected session_id=%s, got %v", sessionID, getBody["session_id"])
	}
	if getBody["state"] != "created" {
		t.Errorf("expected state=created, got %v", getBody["state"])
	}
	if getBody["url"] != "https://github.com/org/repo.git" {
		t.Errorf("expected url, got %v", getBody["url"])
	}
}

func TestGetSession_NotFound(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/nonexistent-id", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeleteSession(t *testing.T) {
	handler := newTestRouter(nil, nil)

	// Create
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d", rr.Code)
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Delete
	rr2 := doRequest(t, handler, http.MethodDelete, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", rr2.Code)
	}

	// Get should 404
	rr3 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr3.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rr3.Code)
	}
}

func TestCreateSession_MissingURL(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"auth_method":"token","token":"ghp_abc"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// --- test connectivity helpers ---

// newTestStorerForWeb creates a storegit.Storer backed by an in-memory SQLite DB.
func newTestStorerForWeb(t *testing.T) *storegit.Storer {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS commit_log (commit_hash TEXT NOT NULL, path TEXT NOT NULL, committed_at INTEGER NOT NULL, message TEXT NOT NULL, operation TEXT NOT NULL DEFAULT '', author_email TEXT NOT NULL DEFAULT '', action TEXT NOT NULL DEFAULT '', PRIMARY KEY (commit_hash, path));
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return storegit.NewStorer(db)
}

// newTestRouterWithGitStore creates a router backed by a real *git.Store.
func newTestRouterWithGitStore(t *testing.T, gs *git.Store) http.Handler {
	t.Helper()
	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	rm.Set("knomit", &RepoInstance{
		Name: "knomit",
		GS:   gs,
		Hub:  hub,
	})
	return NewRouter(rm, nil, false, "kb")
}

// parseSSEEvents parses the SSE response body into a slice of JSON objects.
func parseSSEEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("parse SSE event %q: %v", data, err)
		}
		events = append(events, ev)
	}
	return events
}

// --- test connectivity tests ---

func TestTestConnectivity_SuccessfulClone(t *testing.T) {
	// Create a local knomit store (the "local" repo).
	localStorer := newTestStorerForWeb(t)
	localStore, err := git.InitWithStorer(localStorer, nil, "agent/test")
	if err != nil {
		t.Fatal(err)
	}

	// Write a fact so local has content.
	if _, _, err := localStore.WriteFile("kb/local-fact.md", "# Local\n", "add local", "learn"); err != nil {
		t.Fatal(err)
	}

	// Create a "remote" knomit store with shared history by cloning local.
	// First, advance main to HEAD on local so clone can find it.
	head, err := localStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if err := localStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(head)),
	); err != nil {
		t.Fatal(err)
	}

	// Create a separate "remote" store with content.
	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := remoteStore.WriteFile("kb/remote-fact.md", "# Remote\n", "add remote", "learn"); err != nil {
		t.Fatal(err)
	}
	remoteHead, err := remoteStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatal(err)
	}

	// Register in-process transport.
	loader := server.MapLoader{"inmem:///test-connectivity": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// Build router with the local store.
	handler := newTestRouterWithGitStore(t, localStore)

	// Create a session pointing to the in-memory remote.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///test-connectivity"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Call test connectivity.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("test connectivity: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify SSE content type.
	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	// Parse SSE events.
	events := parseSSEEvents(t, rec.Body.String())
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events (connecting, cloning, done), got %d: %s", len(events), rec.Body.String())
	}

	// Check phases.
	if events[0]["phase"] != "connecting" {
		t.Errorf("first event: expected phase=connecting, got %v", events[0]["phase"])
	}

	// Find the done event.
	var doneEvent map[string]any
	for _, ev := range events {
		if ev["phase"] == "done" {
			doneEvent = ev
			break
		}
	}
	if doneEvent == nil {
		t.Fatalf("no done event found in: %v", events)
	}

	result, ok := doneEvent["result"].(map[string]any)
	if !ok {
		t.Fatalf("done event missing result: %v", doneEvent)
	}

	// Verify result fields.
	if result["default_branch"] == nil || result["default_branch"] == "" {
		t.Error("expected non-empty default_branch")
	}
	if result["history"] == nil {
		t.Error("expected history field")
	}
	if result["remote_fact_count"] == nil {
		t.Error("expected remote_fact_count field")
	}
	remoteCount := int(result["remote_fact_count"].(float64))
	if remoteCount == 0 {
		t.Error("expected non-zero remote_fact_count")
	}
	localCount := int(result["local_fact_count"].(float64))
	if localCount == 0 {
		t.Error("expected non-zero local_fact_count")
	}

	// Verify session state was updated to tested.
	rr2 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("get session: expected 200, got %d", rr2.Code)
	}
	var sessBody map[string]any
	_ = json.NewDecoder(rr2.Body).Decode(&sessBody)
	if sessBody["state"] != "tested" {
		t.Errorf("expected state=tested, got %v", sessBody["state"])
	}
}

func TestTestConnectivity_SessionNotFound(t *testing.T) {
	localStorer := newTestStorerForWeb(t)
	localStore, err := git.InitWithStorer(localStorer, nil, "agent/test")
	if err != nil {
		t.Fatal(err)
	}
	handler := newTestRouterWithGitStore(t, localStore)

	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/nonexistent-id/test", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// --- preview helpers ---

// newTestRouterWithSvcAndGitStore creates a router backed by both a store.Service
// and a *git.Store sharing the same in-memory database.
func newTestRouterWithSvcAndGitStore(t *testing.T) (http.Handler, *git.Store, *store.Service) {
	t.Helper()
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { svc.Close() })

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, "agent/test")
	if err != nil {
		t.Fatalf("git.InitWithStorer: %v", err)
	}

	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	rm.Set("knomit", &RepoInstance{
		Name: "knomit",
		GS:   gs,
		Svc:  svc,
		Hub:  hub,
	})
	return NewRouter(rm, nil, false, "kb"), gs, svc
}

// insertFact inserts a row into the facts table for testing.
func insertFact(t *testing.T, db *sql.DB, path, blobHash, commitHash string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO facts (path, title, blob_hash, type, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES (?, ?, ?, 'observation', '[]', '[]', 0.9, 1, '[]', ?)`,
		path, path, blobHash, commitHash,
	)
	if err != nil {
		t.Fatalf("insertFact %q: %v", path, err)
	}
}

// writeFact writes a fact file to the git store and inserts it into the facts table.
// content must be a valid knomit fact (YAML frontmatter + # Title body).
func writeFact(t *testing.T, gs *git.Store, db *sql.DB, path, content string) {
	t.Helper()
	commitHash, blobHash, err := gs.WriteFile(path, content, "add "+path, "learn")
	if err != nil {
		t.Fatalf("WriteFile %q: %v", path, err)
	}
	insertFact(t, db, path, blobHash, commitHash)
}

// --- preview tests ---

func TestPreview_ComparesLocalAndRemote(t *testing.T) {
	handler, localGS, svc := newTestRouterWithSvcAndGitStore(t)
	db := svc.DB()

	// Local-only fact (with a dead ref and a live ref).
	localOnlyContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: [kb/local-b.md, kb/missing.md]\n---\n# Local A\n\nContent.\n"
	writeFact(t, localGS, db, "kb/local-a.md", localOnlyContent)

	// Local fact that will also be in remote (shared), no dead refs.
	sharedContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Shared\n\nContent.\n"
	writeFact(t, localGS, db, "kb/shared.md", sharedContent)

	// Another local fact (referenced by local-a so it's alive).
	localBContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local B\n\nContent.\n"
	writeFact(t, localGS, db, "kb/local-b.md", localBContent)

	// Build remote store with: shared.md + remote-only.md
	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := remoteStore.WriteFile("kb/shared.md", sharedContent, "add shared", "learn"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := remoteStore.WriteFile("kb/remote-only.md", "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Only\n\nContent.\n", "add remote-only", "learn"); err != nil {
		t.Fatal(err)
	}
	remoteHead, err := remoteStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatal(err)
	}

	// Register in-process transport.
	loader := server.MapLoader{"inmem:///test-preview": remoteStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// Create session and run test connectivity to populate RemoteStore.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///test-preview"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Run test connectivity to advance session to StateTested.
	testReq := httptest.NewRequest(http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	testRec := httptest.NewRecorder()
	handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test connectivity: expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}

	// Run preview.
	previewReq := httptest.NewRequest(http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID+"/preview", nil)
	previewRec := httptest.NewRecorder()
	handler.ServeHTTP(previewRec, previewReq)

	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", previewRec.Code, previewRec.Body.String())
	}

	ct := previewRec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	events := parseSSEEvents(t, previewRec.Body.String())
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (comparing, done), got %d: %s", len(events), previewRec.Body.String())
	}

	if events[0]["phase"] != "comparing" {
		t.Errorf("first event: expected phase=comparing, got %v", events[0]["phase"])
	}

	var doneEvent map[string]any
	for _, ev := range events {
		if ev["phase"] == "done" {
			doneEvent = ev
			break
		}
	}
	if doneEvent == nil {
		t.Fatalf("no done event found in: %v", events)
	}

	result, ok := doneEvent["result"].(map[string]any)
	if !ok {
		t.Fatalf("done event missing result: %v", doneEvent)
	}

	// local has: kb/local-a.md, kb/shared.md, kb/local-b.md (3 facts)
	// remote has: kb/shared.md (+ kb.md init), kb/remote-only.md
	// shared_path: kb/shared.md = 1
	// local_only: kb/local-a.md, kb/local-b.md = 2
	// remote_only: kb/remote-only.md (+ possibly kb.md) >= 1
	// dead_refs_found: kb/missing.md in refs of local-a = 1

	sharedCount := int(result["shared_path"].(float64))
	if sharedCount != 1 {
		t.Errorf("expected shared_path=1, got %d", sharedCount)
	}
	localOnlyCount := int(result["local_only"].(float64))
	if localOnlyCount != 2 {
		t.Errorf("expected local_only=2, got %d", localOnlyCount)
	}
	remoteOnlyCount := int(result["remote_only"].(float64))
	if remoteOnlyCount == 0 {
		t.Errorf("expected remote_only >= 1, got %d", remoteOnlyCount)
	}
	deadRefsCount := int(result["dead_refs_found"].(float64))
	if deadRefsCount != 1 {
		t.Errorf("expected dead_refs_found=1, got %d", deadRefsCount)
	}

	// Verify session state updated to previewed.
	rr2 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("get session: expected 200, got %d", rr2.Code)
	}
	var sessBody map[string]any
	_ = json.NewDecoder(rr2.Body).Decode(&sessBody)
	if sessBody["state"] != "previewed" {
		t.Errorf("expected state=previewed, got %v", sessBody["state"])
	}
}

func TestPreview_SessionNotFound(t *testing.T) {
	handler, _, _ := newTestRouterWithSvcAndGitStore(t)

	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/nonexistent-id/preview", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPreview_WrongState(t *testing.T) {
	handler, _, _ := newTestRouterWithSvcAndGitStore(t)

	// Create a session (state=created, not tested).
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"tok"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Preview without running test first → 409.
	rr2 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID+"/preview", "")
	if rr2.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

// --- apply tests ---

// setupTestedSession creates a handler with a local store+svc, a separate remote
// store (truly disjoint), and manually wires the session into StateTested with
// disjoint history. Returns the handler, sessionID, and SessionManager so tests
// can call the apply endpoint.
func setupTestedSession(t *testing.T) (http.Handler, string) {
	t.Helper()

	// Build local store+svc.
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { svc.Close() })

	localGS, err := git.InitWithStorer(svc.GitStorer(), nil, "agent/test")
	if err != nil {
		t.Fatal(err)
	}

	// Write a local fact.
	factContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Fact\n\nContent.\n"
	writeFact(t, localGS, svc.DB(), "kb/local-fact.md", factContent)

	// Build the remote store with its own independent storer (disjoint history).
	remoteStorer := newTestStorerForWeb(t)
	remoteStore, err := git.InitWithStorer(remoteStorer, nil, "agent/remote")
	if err != nil {
		t.Fatal(err)
	}
	remoteFact := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Fact\n\nContent.\n"
	if _, _, err := remoteStore.WriteFile("kb/remote-fact.md", remoteFact, "add remote", "learn"); err != nil {
		t.Fatal(err)
	}

	// Set up main branch on remote (needed by Replay to create agent branch).
	remoteHead, err := remoteStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteStorer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(remoteHead)),
	); err != nil {
		t.Fatal(err)
	}

	// Build router.
	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	rm.Set("knomit", &RepoInstance{
		Name: "knomit",
		GS:   localGS,
		Svc:  svc,
		Hub:  hub,
	})
	router := NewRouterWithSessionManager(rm, nil, false, "kb", sm)

	// Create session manually and inject it into StateTested with disjoint history.
	sess, err := sm.Create("knomit", "inmem:///test-apply", AuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sess.mu.Lock()
	sess.State = StateTested
	sess.RemoteStore = remoteStore
	sess.TestResult = connectivityResult{
		DefaultBranch: "main",
		History:       "disjoint",
	}
	sess.mu.Unlock()

	return router, sess.ID
}

func TestApply_DisjointHistory_ReplaysLocalFacts(t *testing.T) {
	handler, sessionID := setupTestedSession(t)

	// POST apply with local_wins strategy.
	applyReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`))
	applyRec := httptest.NewRecorder()
	handler.ServeHTTP(applyRec, applyReq)

	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply: expected 200, got %d: %s", applyRec.Code, applyRec.Body.String())
	}

	ct := applyRec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	events := parseSSEEvents(t, applyRec.Body.String())
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %s", len(events), applyRec.Body.String())
	}

	// First event should be replaying phase.
	if events[0]["phase"] != "replaying" {
		t.Errorf("first event: expected phase=replaying, got %v", events[0]["phase"])
	}

	// Find the done event.
	var doneEvent map[string]any
	for _, ev := range events {
		if ev["phase"] == "done" {
			doneEvent = ev
			break
		}
	}
	if doneEvent == nil {
		t.Fatalf("no done event found in: %v", events)
	}

	result, ok := doneEvent["result"].(map[string]any)
	if !ok {
		t.Fatalf("done event missing result: %v", doneEvent)
	}

	// local has 1 fact, remote has at least 1 fact.
	fromLocal := int(result["from_local"].(float64))
	if fromLocal != 1 {
		t.Errorf("expected from_local=1, got %d", fromLocal)
	}
	fromRemote := int(result["from_remote"].(float64))
	if fromRemote == 0 {
		t.Error("expected from_remote > 0")
	}
	totalFacts := int(result["total_facts"].(float64))
	if totalFacts < 2 {
		t.Errorf("expected total_facts >= 2, got %d", totalFacts)
	}

	// Verify session state is applied.
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get session: expected 200, got %d", rr.Code)
	}
	var sessBody map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&sessBody)
	if sessBody["state"] != "applied" {
		t.Errorf("expected state=applied, got %v", sessBody["state"])
	}
}

func TestApply_CanBeCalledMultipleTimes(t *testing.T) {
	handler, sessionID := setupTestedSession(t)

	// First apply with local_wins.
	applyReq1 := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`))
	applyRec1 := httptest.NewRecorder()
	handler.ServeHTTP(applyRec1, applyReq1)

	if applyRec1.Code != http.StatusOK {
		t.Fatalf("first apply: expected 200, got %d: %s", applyRec1.Code, applyRec1.Body.String())
	}

	events1 := parseSSEEvents(t, applyRec1.Body.String())
	var done1 map[string]any
	for _, ev := range events1 {
		if ev["phase"] == "done" {
			done1 = ev
			break
		}
	}
	if done1 == nil {
		t.Fatal("first apply: no done event")
	}

	// Second apply with remote_wins — should also succeed (repeatable).
	applyReq2 := httptest.NewRequest(http.MethodPost,
		"/api/v1/knomit/origin/session/"+sessionID+"/apply",
		strings.NewReader(`{"conflict_strategy":"remote_wins"}`))
	applyRec2 := httptest.NewRecorder()
	handler.ServeHTTP(applyRec2, applyReq2)

	if applyRec2.Code != http.StatusOK {
		t.Fatalf("second apply: expected 200, got %d: %s", applyRec2.Code, applyRec2.Body.String())
	}

	events2 := parseSSEEvents(t, applyRec2.Body.String())
	var done2 map[string]any
	for _, ev := range events2 {
		if ev["phase"] == "done" {
			done2 = ev
			break
		}
	}
	if done2 == nil {
		t.Fatal("second apply: no done event")
	}

	// With remote_wins, from_local should be 0 (all shared paths skip local).
	// But since local and remote have disjoint paths (no shared), both should replay the same.
	// The key test: both calls succeeded without error.
	result1 := done1["result"].(map[string]any)
	result2 := done2["result"].(map[string]any)

	// Both should have the same from_local count (1 local fact, no shared paths).
	if result1["from_local"] != result2["from_local"] {
		t.Errorf("expected same from_local across calls, got %v and %v",
			result1["from_local"], result2["from_local"])
	}

	// Verify session is still in applied state.
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	var sessBody map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&sessBody)
	if sessBody["state"] != "applied" {
		t.Errorf("expected state=applied, got %v", sessBody["state"])
	}
}

func TestApply_SessionNotFound(t *testing.T) {
	handler, _, _ := newTestRouterWithSvcAndGitStore(t)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session/nonexistent-id/apply",
		`{"conflict_strategy":"local_wins"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApply_WrongState(t *testing.T) {
	handler, _, _ := newTestRouterWithSvcAndGitStore(t)

	// Create a session (state=created, not tested).
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"tok"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Apply without running test first -> 409.
	rr2 := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session/"+sessionID+"/apply",
		`{"conflict_strategy":"local_wins"}`)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

func TestTestConnectivity_CloneError(t *testing.T) {
	localStorer := newTestStorerForWeb(t)
	localStore, err := git.InitWithStorer(localStorer, nil, "agent/test")
	if err != nil {
		t.Fatal(err)
	}
	handler := newTestRouterWithGitStore(t, localStore)

	// Register an empty in-process transport so the protocol is known
	// but the endpoint does not exist.
	loader := server.MapLoader{}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// Create session pointing to non-existent remote.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"inmem:///does-not-exist"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Call test connectivity.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID+"/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should still return 200 (SSE stream), but contain an error event.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseSSEEvents(t, rec.Body.String())
	var errorEvent map[string]any
	for _, ev := range events {
		if ev["phase"] == "error" {
			errorEvent = ev
			break
		}
	}
	if errorEvent == nil {
		t.Fatalf("expected error event, got: %v", events)
	}
	if errorEvent["message"] == nil || errorEvent["message"] == "" {
		t.Error("expected non-empty error message")
	}
}

