package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/repos"
)

func newRealManager(t *testing.T) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// newRealManagerWithLocalOriginRoot is newRealManager with LocalOriginRoot set
// to root, so a file:// origin under it clears the local-origin policy gate.
// newRealManager itself must keep no root configured — other tests depend on
// filesystem origins being disabled by default — so this is a separate helper
// rather than a parameter added to it.
func newRealManagerWithLocalOriginRoot(t *testing.T, root string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestPostRepos_AcceptsAndCreates pins the 202 contract: POST answers
// immediately with the job's identity and a Location pointing at the poll
// resource, and the create it started really does produce the repo.
//
// It replaces the old NDJSON-stream test. The stream is gone on purpose — a
// response that streamed until the create finished was the mechanism by which
// the request OWNED the work (issue #67); 202 removes the ownership rather
// than patching around it.
func TestPostRepos_AcceptsAndCreates(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad 202 body %q: %v", rec.Body.String(), err)
	}
	id, _ := body["create_id"].(string)
	if id == "" {
		t.Fatalf("202 body carries no create_id: %v", body)
	}
	if got := rec.Header().Get("Location"); !strings.HasSuffix(got, "/repo-creates/"+id) {
		t.Fatalf("Location = %q, want a /repo-creates/%s URL", got, id)
	}
	// The 202 is answered BEFORE the work is done, which is the whole point:
	// state is "running", not a result.
	if body["state"] != "running" {
		t.Fatalf("state = %v, want running", body["state"])
	}

	final := awaitCreate(t, r, rec)
	if final["state"] != "done" {
		t.Fatalf("final state = %v", final["state"])
	}
	repo, _ := final["repo"].(map[string]any)
	if repo == nil || repo["name"] != "work" {
		t.Fatalf("terminal poll carries no repo: %v", final)
	}
	if s.Manager.Get("work") == nil {
		t.Fatal("repo not registered")
	}
}

// TestGetRepoCreate_UnknownIDIs404 pins the poll resource's refusal. Unknown
// and expired are deliberately one answer — see handleHALRepoCreateStatus.
func TestGetRepoCreate_UnknownIDIs404(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repo-creates/nosuchjob", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestPostRepos_ConflictOnExistingName(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "work")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestPostRepos_BadNameReturns400(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"Bad Name","mode":"preset"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// An initialize against a remote with NO branches must be refused BEFORE the
// NDJSON stream opens, as a real 409 — the status the OpenAPI documents for
// this case. Unless CreatePreflight probes, ErrRemoteNoBranches can only be
// produced inside Create, i.e. after w.WriteHeader(200), which would make
// createErrStatus's case unreachable and the documented 409 a lie.
//
// This is the INVERSE of the check it replaces. The deleted "seed" mode
// required a ref-less remote and 409'd one with refs; initialize requires a
// branch to cut its agent branch from and 409s one without.
func TestPostRepos_InitializeEmptyRemoteIs409NotAStreamedError(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if err := exec.Command("git", "init", "--bare", remote).Run(); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	body := `{"name":"kb","mode":"initialize","ontology_preset":"default","origin":{"url":"file://` + remote + `"}}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "ndjson") {
		t.Fatalf("a pre-stream refusal must not open the NDJSON stream; content-type = %q", ct)
	}
	// The body must carry the INSTRUCTION, not just the diagnosis — a reader
	// who is told "no branches" and not told that one commit fixes it has to
	// guess what knomit wants from them.
	if !strings.Contains(rec.Body.String(), "one commit is enough") {
		t.Fatalf("body does not tell the user how to fix it: %s", rec.Body.String())
	}
}

// MaxOntologyBytes claims to cap "an ontology accepted by :validate and by
// create". The create path decoded its body with no MaxBytesReader at all, so
// ontology_yaml was unbounded there and the comment asserted a guard that did
// not exist.
func TestPostRepos_OversizeBodyIs413(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	huge := strings.Repeat("x", MaxOntologyBytes+1)
	body := `{"name":"kb","mode":"custom","ontology_yaml":"` + huge + `"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if s.Manager.Get("kb") != nil {
		t.Fatal("an oversize create must not register a repo")
	}
}

// The cap must not be so tight that an ordinary create trips it: a body just
// under the limit still gets its normal answer (here, a 400 for a body that is
// valid JSON but not a valid ontology) rather than a 413.
func TestPostRepos_BodyUnderTheCapIsNotRejectedAsOversize(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	// Comment padding keeps the YAML valid while making the body large.
	yaml := "id: x\\nname: X\\ntopics:\\n  alpha:\\n    description: d\\n" +
		strings.Repeat("# pad\\n", 1000)
	body := `{"name":"kb","mode":"custom","ontology_yaml":"` + yaml + `"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a body well under the cap was rejected as oversize: %s", rec.Body.String())
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// The two paths have to agree on what MaxOntologyBytes measures. Create used to
// cap the whole JSON ENVELOPE at that number, but ontology_yaml is a JSON
// string inside it and escaping inflates the YAML — an ontology is mostly
// newlines, and each costs two bytes encoded. So an ontology comfortably under
// the documented limit, which :validate had just shown green in the editor,
// came back from create as 413 "Ontology too large": an error naming a limit
// the document does not exceed, with no action the user could take.
func TestPostRepos_YAMLUnderTheCapWhoseJSONEncodingExceedsItIsAccepted(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	// Newline-dense, so the encoded form is ~2x the raw form: raw stays under
	// MaxOntologyBytes while the escaped body lands well over it.
	head := "id: x\nname: X\ntopics:\n  alpha:\n    description: d\n"
	raw := head + strings.Repeat("# p\n", (MaxOntologyBytes-len(head)-1024)/4)
	if len(raw) > MaxOntologyBytes {
		t.Fatalf("test fixture is %d bytes, over the cap it is meant to stay under", len(raw))
	}
	encoded, err := json.Marshal(map[string]string{"name": "kb", "mode": "custom", "ontology_yaml": raw})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= MaxOntologyBytes {
		t.Fatalf("encoded body is %d bytes, not over the cap — the fixture proves nothing", len(encoded))
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", bytes.NewReader(encoded)))

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("an ontology of %d raw bytes (under the %d cap) was rejected as oversize: %s",
			len(raw), MaxOntologyBytes, rec.Body.String())
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// The other side of the same boundary: the cap still applies, and it applies to
// the DECODED ontology rather than to the envelope around it.
func TestPostRepos_OversizeOntologyYAMLIs413(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	encoded, err := json.Marshal(map[string]string{
		"name": "kb", "mode": "custom", "ontology_yaml": strings.Repeat("x", MaxOntologyBytes+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", bytes.NewReader(encoded)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Ontology too large") {
		t.Fatalf("413 does not name the ontology as the cause: %s", rec.Body.String())
	}
	if s.Manager.Get("kb") != nil {
		t.Fatal("an oversize create must not register a repo")
	}
}

// The other three shape refusals, each of which createErrStatus maps to a 409
// and openapi.yaml documents as one — and none of which could happen before
// the stream opened, because CreatePreflight probed for exactly one condition
// (Empty) in exactly one mode (initialize).
//
// A refusal that arrives INSIDE the NDJSON stream is a 200 with an untyped
// error line: the status code says the create was accepted, the documented 409
// never occurs, and a client cannot tell "your remote is the wrong shape for
// this mode" from "the create broke halfway through". These pin all three at
// the door.

// clone joins an existing knowledge base, and refuses a supplied ontology on
// the grounds that the remote's own governs — so a remote with no ontology
// leaves the mode nothing to honour. Before this, the clone was accepted and
// only refused mid-stream.
func TestPostRepos_CloneOfANonKnowledgeBaseIs409NotAStreamedError(t *testing.T) {
	root := t.TempDir()
	url := seedPlainRemoteForTest(t, filepath.Join(root, "remote.git"))

	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	body := `{"name":"kb","mode":"clone","origin":{"url":"` + url + `","branch":"main"}}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "ndjson") {
		t.Fatalf("a pre-stream refusal must not open the NDJSON stream; content-type = %q", ct)
	}
	// It must name the mode that WOULD have worked. Every one of these three
	// refusals has one, and a reader told only what is wrong has to guess.
	if !strings.Contains(rec.Body.String(), "initialize") {
		t.Fatalf("body does not name the mode that works: %s", rec.Body.String())
	}
}

// The mirror: initialize refuses a branch that is already a knowledge base
// rather than writing a second ontology over the one that governs it.
func TestPostRepos_InitializeOfAKnowledgeBaseIs409NotAStreamedError(t *testing.T) {
	root := t.TempDir()
	url := seedKnomitRemoteForTest(t, filepath.Join(root, "remote.git"), "seed")

	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	body := `{"name":"kb","mode":"initialize","ontology_preset":"default","origin":{"url":"` + url + `","branch":"main"}}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clone") {
		t.Fatalf("body does not name the mode that works: %s", rec.Body.String())
	}
}

// A ref-less remote is refused for CLONE too, not only for initialize.
// openapi.yaml documents this 409 as applying to "mode=initialize and
// mode=clone alike"; only one of those was true.
func TestPostRepos_CloneOfARefLessRemoteIs409NotAStreamedError(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if err := exec.Command("git", "init", "--bare", remote).Run(); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	body := `{"name":"kb","mode":"clone","origin":{"url":"file://` + remote + `"}}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "one commit is enough") {
		t.Fatalf("body does not tell the user how to fix it: %s", rec.Body.String())
	}
}

// TestPostRepos_ClientDisconnectDoesNotAbortTheCreate is the web-layer half of
// issue #67: the repo's creation must not be the property of the request that
// asked for it.
//
// Before the fix the handler passed r.Context() straight into Manager.Create,
// so a client that went away cancelled the create at its next step boundary —
// discarding, in clone mode, a network fetch that may already have completed,
// and making the outcome of a slow create depend on whether a browser tab
// stayed open.
//
// Under 202 the request cannot own the work by construction, so what this test
// guards is that nothing SNEAKS the request's context back in — the preflight
// still legitimately receives it, and a future change that also handed it to
// the create would be invisible to every other test here.
//
// The request context is cancelled BEFORE the handler runs, which makes the
// test deterministic rather than a race against a fast create: there is no
// window in which the create could have finished first.
func TestPostRepos_ClientDisconnectDoesNotAbortTheCreate(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`)).WithContext(ctx)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}

	// The repo is created regardless. It is NOT registered by the instant the
	// handler returns — 202 answers before the work is done — so this waits on
	// the effect rather than assuming otherwise.
	deadline := time.Now().Add(60 * time.Second)
	for s.Manager.Get("work") == nil {
		if time.Now().After(deadline) {
			t.Fatalf("repo was never created: the create is still tied to the request context")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGetRepoCreate_TimeoutIsReadableAsATerminalFailure is the HTTP-visible
// half of requirement (2): when a detached create exceeds its own deadline,
// the client that comes back to ask does not find "still running" or a
// half-created repo — it finds a terminal failure that says it timed out, and
// the repo genuinely is not there.
//
// This is what the 202 shape buys that a stream could not: the outcome of a
// create outlives the connection that started it.
func TestGetRepoCreate_TimeoutIsReadableAsATerminalFailure(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
		// Already expired when the create's goroutine runs.
		CreateTimeout: time.Nanosecond,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	var accepted map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("bad 202 body: %v", err)
	}
	id, _ := accepted["create_id"].(string)

	deadline := time.Now().Add(60 * time.Second)
	var st map[string]any
	for {
		poll := httptest.NewRecorder()
		r.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/repo-creates/"+id, nil))
		if poll.Code != http.StatusOK {
			t.Fatalf("poll status = %d, body=%s", poll.Code, poll.Body.String())
		}
		if err := json.Unmarshal(poll.Body.Bytes(), &st); err != nil {
			t.Fatalf("bad poll body: %v", err)
		}
		if st["state"] != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create never became terminal: %v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if st["state"] != "failed" {
		t.Fatalf("state = %v, want failed", st["state"])
	}
	if st["timed_out"] != true {
		t.Fatalf("timed_out = %v, want true — a deadline must be distinguishable from a create error", st["timed_out"])
	}
	if msg, _ := st["error"].(string); msg == "" {
		t.Fatalf("failed poll carries no error text: %v", st)
	}
	if _, ok := st["repo"]; ok {
		t.Fatalf("a failed create must not offer a repo link: %v", st)
	}
	if m.Get("work") != nil {
		t.Fatal("the timed-out repo must not be registered")
	}
}

// TestReposNamedCreatesStaysReachable pins the reason the create-status route
// is /repo-creates/{id} and not /repos/creates/{id}.
//
// Repo names use the alphabet [a-z0-9_-], so "creates" is a legal repo name.
// chi prefers a static path segment over a {param} edge and does NOT backtrack
// when the static branch fails to match the rest of the path — so a
// /repos/creates/... route would not merely shadow one URL, it would make a
// repo actually called "creates" unreachable at EVERY route under
// /repos/{repo}. A sibling collection has no such shadow, and this test fails
// the moment someone nests it back.
func TestReposNamedCreatesStaysReachable(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "creates")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/creates/branches", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /repos/creates/branches = %d, want 200 — a repo named "+
			"\"creates\" is shadowed by the create-status route; body=%s",
			rec.Code, rec.Body.String())
	}
}
