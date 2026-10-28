package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/edge/web/hal"
)

// A POST mints a path under the server's ontology root from the domain,
// commits the fact there on the REQUEST branch with the create message, and
// answers 201 with a Location naming that path on that branch. Sabotage: write
// on ri.AgentBranch(), ignore the ontology root, or change the message.
func TestHandleFactCreate_Returns201WithLocation(t *testing.T) {
	fx := newFactWriteFixture(t, "know")

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":["ai","ml"],"confidence":0.9,"sources":1}`
	rec := fx.do(http.MethodPost, "/repos/alpha/branches/"+restWriteURL+"/facts", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	w := fx.wrote()
	if len(w) != 1 || !strings.HasPrefix(w[0], "know/ai/ml/") {
		t.Fatalf("wrote %v; want one fact under know/ai/ml/", w)
	}
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "/repos/alpha/branches/"+restWriteURL+"/facts/"+w[0]) {
		t.Errorf("Location: got %q, want the written path %s on %s", loc, w[0], restWriteURL)
	}
	if ct := rec.Header().Get("Content-Type"); ct != hal.ContentType {
		t.Errorf("Content-Type: got %q, want %q", ct, hal.ContentType)
	}
	stored, ok := fx.read(restWriteBranch, w[0])
	if !ok || !strings.Contains(stored, "# My Fact") {
		t.Errorf("the request branch holds %q at %s; want the created fact", stored, w[0])
	}
	if _, ok := fx.read(fsAgentBranch, w[0]); ok {
		t.Errorf("the fact also landed on the agent branch")
	}
	if msg, op := fx.head(restWriteBranch); msg != "create: My Fact via API" || op != "update" {
		t.Errorf("commit: message %q operation %q; want %q, %q", msg, op, "create: My Fact via API", "update")
	}

	var view struct {
		Title  string      `json:"title"`
		Domain []string    `json:"domain"`
		Type   string      `json:"type"`
		Links  hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Title != "My Fact" {
		t.Errorf("title: got %q, want %q", view.Title, "My Fact")
	}
	if _, ok := view.Links["self"]; !ok {
		t.Error("missing self link")
	}
}

// A store write that fails is a 500 naming the store's error, with no
// Location. Sabotage: drop the write error check in handleFactCreate.
func TestHandleFactCreate_WriteError_Returns500(t *testing.T) {
	fx := newFactWriteFixture(t, "know")
	injected := errors.New("w0h: injected create fault")
	var hits []string
	t.Cleanup(fx.svc.Facts().SetWriteFaultForTest(func(path string) error {
		hits = append(hits, path)
		return injected
	}))

	rec := fx.do(http.MethodPost, "/repos/alpha/branches/"+restWriteURL+"/facts", `{"title":"T","body":"b","domain":["ai","ml"]}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if p := writeProblem(t, rec); p.Title != "Failed to write fact" || !strings.Contains(p.Detail, injected.Error()) {
		t.Errorf("problem %q / %q; want the write failure naming %q", p.Title, p.Detail, injected)
	}
	if len(hits) != 1 || !strings.HasPrefix(hits[0], "know/ai/ml/") {
		t.Errorf("write fault hits %v; want one, at the minted path", hits)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location %q on a failed create", loc)
	}
}

func TestHandleFactCreate_MissingTitle_Returns400(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
	}
	r := s.NewAPIRouter()

	body := `{"body":"no title here"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// A whitespace-only title is as empty as a missing one: it serialises as a
// bare "# " heading that no reader can parse.
func TestHandleFactCreate_WhitespaceTitle_Returns400(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
	}
	r := s.NewAPIRouter()

	body := `{"title":"   ","body":"blank title"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleFactCreate_MultilineTitle_Returns400(t *testing.T) {
	for _, title := range []string{`\nFoo`, `Foo\nBar`, `Foo\r\nBar`} {
		s := &Server{
			Manager:      newTestManagerWithRepos(t, "alpha"),
			OntologyRoot: "know",
		}
		r := s.NewAPIRouter()
		body := `{"title":"` + title + `","body":"b"}`
		rec := httptest.NewRecorder()
		req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("title %q: status %d, want 400, body=%s", title, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleFactCreate_TrimsTitleEdges(t *testing.T) {
	fx := newFactWriteFixture(t, "know")
	r := fx.s.NewAPIRouter()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+restWriteURL+"/facts",
		strings.NewReader(`{"title":"  Padded  ","body":"b"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Title != "Padded" {
		t.Errorf("title: got %q, want %q", view.Title, "Padded")
	}
}

func TestHandleFactCreate_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "know",
	}
	r := s.NewAPIRouter()

	body := `{"title":"T"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/missing/branches/agent:test/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestHandleFactCreate_InvalidType_Returns400(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
	}
	r := s.NewAPIRouter()

	body := `{"title":"T","type":"bogus"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestHandleListSessions_EmptyList_Returns200(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions", nil))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Sessions []map[string]any `json:"sessions"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 || len(body.Embedded.Sessions) != 0 {
		t.Errorf("expected empty collection, got count=%d items=%d",
			body.Count, len(body.Embedded.Sessions))
	}
}

func TestHandleListSessions_AfterCreate_Returns1(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	// Create a session.
	createBody := `{"url":"https://github.com/example/repo.git"}`
	createRec := httptest.NewRecorder()
	createReq := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions", strings.NewReader(createBody)))
	createReq.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body=%s", createRec.Code, createRec.Body.String())
	}

	// List sessions.
	listRec := httptest.NewRecorder()
	listReq := fromLoopback(httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions", nil))
	r.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body=%s", listRec.Code, listRec.Body.String())
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Sessions []map[string]any `json:"sessions"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 1 || len(body.Embedded.Sessions) != 1 {
		t.Errorf("expected 1 session, got count=%d items=%d",
			body.Count, len(body.Embedded.Sessions))
	}
}

func TestHandleListSessions_UnknownRepo_Returns404(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/origin-sessions", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestGetOpenAPISpec_Returns200(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type: got %q, want application/yaml", ct)
	}
}

// TestHandleFactCreate_AppliesDefaults pins the same contract knomit_learn
// enforces: a create body that omits confidence and sources must land on the
// documented defaults rather than Go's zero values. A fact written with
// sources 0 contributes nothing to any downstream evidence weight, which
// multiplies by sources.
func TestHandleFactCreate_AppliesDefaults(t *testing.T) {
	fx := newFactWriteFixture(t, "know")
	r := fx.s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":["ai"]}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+restWriteURL+"/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Confidence float64 `json:"confidence"`
		Sources    int     `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Sources != 1 {
		t.Errorf("omitted sources: got %d, want the default 1", view.Sources)
	}
	if view.Confidence != 0.7 {
		t.Errorf("omitted confidence: got %v, want the default 0.7", view.Confidence)
	}
}

// TestHandleFactCreate_ExplicitZeroSourcesSurvives guards the other side: an
// explicit 0 is a legal value and must not be defaulted away.
func TestHandleFactCreate_ExplicitZeroSourcesSurvives(t *testing.T) {
	fx := newFactWriteFixture(t, "know")
	r := fx.s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","sources":0,"confidence":0}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+restWriteURL+"/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Confidence float64 `json:"confidence"`
		Sources    int     `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Sources != 0 {
		t.Errorf("explicit 0 sources: got %d, want 0 preserved", view.Sources)
	}
	if view.Confidence != 0 {
		t.Errorf("explicit 0 confidence: got %v, want 0 preserved", view.Confidence)
	}
}

// The create path derives the fact's location from caller-supplied domain, so
// a caller can steer it into a private directory. Accepting that returns 201
// for a fact the indexer, Verify and the exporter all skip — silent loss.
func TestHandleFactCreate_RejectsPrivateDomain(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
	}
	r := s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":[".secret"],"confidence":0.9,"sources":1}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "private") {
		t.Errorf("problem body should name the private-path rule, got %s", rec.Body.String())
	}
}
