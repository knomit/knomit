package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/model"
)

// REST writes are accepted on the agent branch and on this instance's own
// experiments only. The write fixture's request branch is an experiment, so it
// is never the agent branch: a handler that wrote, deleted, resolved refs or
// read the prior version on ri.AgentBranch() lands on the wrong branch, and the
// fixture's same-path decoys on the agent branch say so.
const (
	restWriteBranch = "exp/rest-write"
	restWriteURL    = "exp:rest-write"
)

// factWriteFixture is a REAL store behind the REST fact write handlers (POST,
// PUT and DELETE). Nothing is stubbed: the handlers write through the store,
// and the store's own WriteFact / DeleteFact hooks (fail-only, installed
// passing) record the path of every write and delete that reaches the store,
// so a test can assert that a refused request never reached git.
type factWriteFixture struct {
	t   *testing.T
	s   *Server
	svc *store.Service

	mu      sync.Mutex
	seeding bool
	writes  []string // paths WriteFact was called with by a request
	deletes []string // paths DeleteFact was called with by a request
}

// newFactWriteFixture opens the store, the experiment restWriteBranch forked
// from the agent branch, and a Server whose ontology root is root. A root
// other than "kb" is set on the store too, so the store indexes the facts the
// handlers write there.
func newFactWriteFixture(t *testing.T, root string) *factWriteFixture {
	t.Helper()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	if root != "kb" {
		svc.SetOntologyRoot(root)
	}
	f := &factWriteFixture{t: t, svc: svc, s: &Server{Manager: m, OntologyRoot: root}}
	f.seed(fsAgentBranch, root+"/seed.md", factText(t, root+"/seed.md", "Seed", 0.9, nil))
	openRESTExperiment(t, svc, "rest-write")

	t.Cleanup(svc.Facts().SetWriteFaultForTest(func(path string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.seeding {
			f.writes = append(f.writes, path)
		}
		return nil
	}))
	t.Cleanup(svc.Facts().SetDeleteFaultForTest(func(path string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deletes = append(f.deletes, path)
		return nil
	}))
	return f
}

// seed writes content at path on branch directly through the store, not
// through a handler, and is not recorded as a request's write.
func (f *factWriteFixture) seed(branch, path, content string) {
	f.t.Helper()
	f.mu.Lock()
	f.seeding = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.seeding = false
		f.mu.Unlock()
	}()
	_, err := f.svc.Facts().WriteFact(context.Background(), branch, path, content, "seed "+path, "learn")
	require.NoError(f.t, err)
}

// wrote and deleted are the paths requests wrote or deleted, in order.
func (f *factWriteFixture) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *factWriteFixture) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

// do serves one request on the API router from loopback. A non-empty body is
// sent as JSON.
func (f *factWriteFixture) do(method, url, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(method, url, rd))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	f.s.NewAPIRouter().ServeHTTP(rec, req)
	return rec
}

// factURL is the fact resource at path on the fixture's request branch.
func (f *factWriteFixture) factURL(path string) string {
	return "/repos/alpha/branches/" + restWriteURL + "/facts/" + path
}

// read is the content at path at the head of branch, and whether it is there.
func (f *factWriteFixture) read(branch, path string) (string, bool) {
	f.t.Helper()
	res, err := f.svc.Facts().ReadFact(context.Background(), branch, path, nil)
	if errors.Is(err, model.ErrPathNotFound) {
		return "", false
	}
	require.NoError(f.t, err)
	return res.Content, true
}

// head is the newest commit on branch: its message and operation are what the
// handler asked the store to record.
func (f *factWriteFixture) head(branch string) (message, operation string) {
	f.t.Helper()
	entries, _, _, err := f.svc.HistoryQuery().LogPaginated(context.Background(), branch, "", 1, "", "", "")
	require.NoError(f.t, err)
	require.NotEmpty(f.t, entries)
	return entries[0].Message, entries[0].Operation
}

// writeProblem decodes a write handler's problem document.
func writeProblem(t *testing.T, rec *httptest.ResponseRecorder) hal.Problem {
	t.Helper()
	var p hal.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	return p
}

// testFactContent is valid fact markdown: frontmatter + # heading body.
const testFactContent = "---\\ntype: observation\\ndomain: [ai]\\nconfidence: 0.9\\nsources: 1\\n---\\n\\n# Test Fact\\n\\nBody text.\\n"

// A PUT commits the client's bytes at the request's path on the REQUEST
// branch, with the update message, and leaves the same path on the agent
// branch alone. Sabotage: write on ri.AgentBranch(), at another path, with
// another message or operation.
func TestHandleFactUpdate_Returns200WithHAL(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	decoy := factText(t, "kb/ai/test.md", "Agent decoy", 0.9, nil)
	f.seed(fsAgentBranch, "kb/ai/test.md", decoy)

	rec := f.do(http.MethodPut, f.factURL("kb/ai/test.md"), `{"content":"`+testFactContent+`"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	require.Equal(t, []string{"kb/ai/test.md"}, f.wrote())
	got, ok := f.read(restWriteBranch, "kb/ai/test.md")
	require.True(t, ok, "the fact is on the request branch")
	require.Equal(t, unescape(testFactContent), got)
	agent, _ := f.read(fsAgentBranch, "kb/ai/test.md")
	require.Equal(t, decoy, agent, "the agent branch's copy is untouched")
	msg, op := f.head(restWriteBranch)
	require.Equal(t, "edit: update kb/ai/test.md via API", msg)
	require.Equal(t, "update", op)
}

func TestHandleFactUpdate_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	body := `{"content":"anything"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut,
		"/repos/missing/branches/agent:test/facts/know/ai/test.md",
		strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// A store write that fails is a 500 naming the store's error, and the fact is
// not there. The hook fails exactly the requested path and counts its hits.
func TestHandleFactUpdate_WriteError_Returns500(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	injected := errors.New("w0h: injected write fault")
	var hits []string
	t.Cleanup(f.svc.Facts().SetWriteFaultForTest(func(path string) error {
		hits = append(hits, path)
		if path == "kb/ai/test.md" {
			return injected
		}
		return nil
	}))

	// Content must parse: validation runs before the write, so unparseable
	// content would short-circuit to 422 and never reach the store.
	rec := f.do(http.MethodPut, f.factURL("kb/ai/test.md"), `{"content":"`+testFactContent+`"}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	p := writeProblem(t, rec)
	require.Equal(t, "Failed to write fact", p.Title)
	require.Contains(t, p.Detail, injected.Error())
	require.Equal(t, []string{"kb/ai/test.md"}, hits)
	_, ok := f.read(restWriteBranch, "kb/ai/test.md")
	require.False(t, ok, "a failed write leaves nothing behind")
}

// Unlike the create handlers, which derive their target path from
// topic/category, PUT receives the path verbatim from the caller: the path is
// the one thing the client fully controls, and this is a create too — see
// priorRefs' "no prior version" comment — so a private segment here would
// commit a fact to git that every reader (indexer, Verify, the OKF exporter)
// then permanently skips.
func TestHandleFactUpdate_RejectsPrivatePath(t *testing.T) {
	f := newFactWriteFixture(t, "kb")

	rec := f.do(http.MethodPut, f.factURL("kb/.drafts/test.md"), `{"content":"`+testFactContent+`"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "private") {
		t.Errorf("problem body should name the private-path rule, got %s", rec.Body.String())
	}
	if w := f.wrote(); len(w) != 0 {
		t.Errorf("the store wrote %v for a private path; it must never reach git", w)
	}
}

// TestFactWrite_AllowsArtifactPath: PUT is the REST twin of knomit_update,
// taking a fully caller-supplied path, so it accepts what the MCP guard
// accepts (internal/edge/mcp/update.go) — artifacts/<area>/ is the agents' working
// -file root, writable though excluded from discovery (F25). The handler
// upserts (priorRefs returns nil for a fresh path, treated as "no prior
// version" rather than an error), so this PUT both creates the file and proves
// the write reaches the store.
func TestFactWrite_AllowsArtifactPath(t *testing.T) {
	f := newFactWriteFixture(t, "kb")

	rec := f.do(http.MethodPut, f.factURL("artifacts/jobs/ae/crawl-state.md"), `{"content":"`+testFactContent+`"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"artifacts/jobs/ae/crawl-state.md"}, f.wrote())
	require.Contains(t, rec.Body.String(), "Body text.", "the response echoes the written content")
	got, ok := f.read(restWriteBranch, "artifacts/jobs/ae/crawl-state.md")
	require.True(t, ok)
	require.Equal(t, unescape(testFactContent), got)
}

// TestFactWrite_RefusesOtherPrivatePaths: a private path must be refused,
// with the same status/envelope as TestHandleFactUpdate_RejectsPrivatePath and
// a message that names artifacts/<area>/ as where working files go instead.
func TestFactWrite_RefusesOtherPrivatePaths(t *testing.T) {
	f := newFactWriteFixture(t, "kb")

	rec := f.do(http.MethodPut, f.factURL("kb/.drafts/x.md"), `{"content":"`+testFactContent+`"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	// Decode rather than substring-match the raw body: encoding/json HTML-escapes
	// '<' and '>' by default, so the wire form is "artifacts/<area>/".
	if p := writeProblem(t, rec); !strings.Contains(p.Detail, "artifacts/<area>/") {
		t.Errorf("problem detail should name artifacts/<area>/, got %q", p.Detail)
	}
	if w := f.wrote(); len(w) != 0 {
		t.Errorf("the store wrote %v for a refused private path; it must never reach git", w)
	}
}

// A DELETE retracts the path on the REQUEST branch with the retract message;
// the same path on the agent branch, and a sibling on the request branch, stay.
// Sabotage: delete on ri.AgentBranch(), another path, another message, or
// write instead of delete.
func TestHandleFactDelete_Returns204(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	f.seed(restWriteBranch, "kb/ai/test.md", factText(t, "kb/ai/test.md", "On X", 0.9, nil))
	f.seed(restWriteBranch, "kb/ai/other.md", factText(t, "kb/ai/other.md", "Sibling", 0.9, nil))
	f.seed(fsAgentBranch, "kb/ai/test.md", factText(t, "kb/ai/test.md", "Agent decoy", 0.9, nil))

	rec := f.do(http.MethodDelete, f.factURL("kb/ai/test.md"), "")

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Equal(t, []string{"kb/ai/test.md"}, f.deleted())
	require.Empty(t, f.wrote(), "a delete writes nothing")
	_, ok := f.read(restWriteBranch, "kb/ai/test.md")
	require.False(t, ok, "the fact is gone from the request branch")
	_, ok = f.read(restWriteBranch, "kb/ai/other.md")
	require.True(t, ok, "its sibling stays")
	_, ok = f.read(fsAgentBranch, "kb/ai/test.md")
	require.True(t, ok, "the agent branch's copy stays")
	msg, op := f.head(restWriteBranch)
	require.Equal(t, "manual-review: retract kb/ai/test.md", msg)
	require.Equal(t, "retract", op)
}

// A store delete that fails with anything but "not there" is a 500 naming the
// store's error, and the fact stays.
func TestHandleFactDelete_StoreError_Returns500(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	f.seed(restWriteBranch, "kb/ai/test.md", factText(t, "kb/ai/test.md", "On X", 0.9, nil))
	injected := errors.New("w0h: injected delete fault")
	var hits []string
	t.Cleanup(f.svc.Facts().SetDeleteFaultForTest(func(path string) error {
		hits = append(hits, path)
		return injected
	}))

	rec := f.do(http.MethodDelete, f.factURL("kb/ai/test.md"), "")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	p := writeProblem(t, rec)
	require.Equal(t, "Failed to delete fact", p.Title)
	require.Contains(t, p.Detail, injected.Error())
	require.Equal(t, []string{"kb/ai/test.md"}, hits)
	_, ok := f.read(restWriteBranch, "kb/ai/test.md")
	require.True(t, ok, "a failed delete leaves the fact")
}

// DELETE is a write, and it is the REST twin of knomit_retract — which this
// branch gated on exactly that reasoning. The handler takes the path verbatim
// and the store performs no fact-shape check, so without this guard the
// endpoint will happily remove kb/.drafts/x.md or .knomit/ontology.yaml.
// Same condition, status and problem envelope as handleFactUpdate.
func TestHandleFactDelete_RejectsPrivatePath(t *testing.T) {
	for _, path := range []string{"kb/.drafts/x.md", ".knomit/ontology.yaml", ".github/workflows/ci.yml",
		// F25: the former agent areas are closed too.
		".knomit/jobs/ae/crawl-state.md", ".knomit/skills/s/extra.md"} {
		t.Run(path, func(t *testing.T) {
			f := newFactWriteFixture(t, "kb")

			rec := f.do(http.MethodDelete, f.factURL(path), "")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
			// Decode rather than substring-match: encoding/json HTML-escapes
			// '<' and '>', so the wire form is "artifacts/<area>/".
			if p := writeProblem(t, rec); !strings.Contains(p.Detail, "artifacts/<area>/") {
				t.Errorf("problem detail should name artifacts/<area>/, got %q", p.Detail)
			}
			if d := f.deleted(); len(d) != 0 {
				t.Errorf("the store deleted %v for a private path; it must never reach git", d)
			}
		})
	}
}

// What the guard must preserve: a job's own state under artifacts/<area>/ is
// writable, and deleting it is a write like any other.
func TestHandleFactDelete_AllowsArtifactPath(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	f.seed(restWriteBranch, "artifacts/jobs/ae/crawl-state.md", unescape(testFactContent))

	rec := f.do(http.MethodDelete, f.factURL("artifacts/jobs/ae/crawl-state.md"), "")

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Equal(t, []string{"artifacts/jobs/ae/crawl-state.md"}, f.deleted())
	_, ok := f.read(restWriteBranch, "artifacts/jobs/ae/crawl-state.md")
	require.False(t, ok)
}

func TestHandleFactDelete_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodDelete,
		"/repos/missing/branches/agent:test/facts/know/ai/test.md", nil))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// TestHandleFactDelete_MissingPath_Returns404 (F25 follow-up): DELETE of a
// fact path that is not there — plain, or with a segment percent-encoded —
// answers 404 "Fact not found" naming the path and the REQUEST branch, exactly
// as a GET of the same missing path does, instead of 500. Against the REAL
// store: the bug was in deleteFile's own existence check
// (internal/engine/store/fact_write.go), which returned a bare error string
// writeStoreError's switch could not recognise, so EVERY missing-path delete —
// encoded or not — fell through to the default 500 case. The path exists on
// the agent branch, so a delete there would succeed.
// Sabotage: drop the wrapped store.ErrPathNotFound from deleteFile's
// existence check, drop its translation in deleteFact or the errFactNotFound
// check in handleFactDelete (500), change the title, or delete on the agent
// branch (204).
func TestHandleFactDelete_MissingPath_Returns404(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	f.seed(fsAgentBranch, "kb/verdicts/missing.md", factText(t, "kb/verdicts/missing.md", "Agent only", 0.9, nil))

	for _, path := range []string{
		"kb/verdicts/missing.md",
		"kb/verdicts%2Fmissing.md", // the slash itself percent-encoded
	} {
		rec := f.do(http.MethodDelete, f.factURL(path), "")
		require.Equal(t, http.StatusNotFound, rec.Code, "DELETE %s: %s", path, rec.Body.String())
		require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"), "DELETE %s", path)
		p := writeProblem(t, rec)
		require.Equal(t, "Fact not found", p.Title, "DELETE %s", path)
		// The detail echoes the path as the request spelled it.
		require.Equal(t, `no fact at path "`+path+`" on branch "`+restWriteBranch+`"`, p.Detail, "DELETE %s", path)
	}
	_, ok := f.read(fsAgentBranch, "kb/verdicts/missing.md")
	require.True(t, ok, "the agent branch's copy stays")
}

// TestHandleFactUpdate_InvalidContent_RejectsBeforeWriting: content that
// ParseFact refuses must be rejected with 422 WITHOUT ever reaching git.
// Validating after the write commits the bad blob as the branch HEAD for that
// path while telling the client the write failed — every later read of that
// path then fails or silently drops it.
func TestHandleFactUpdate_InvalidContent_RejectsBeforeWriting(t *testing.T) {
	f := newFactWriteFixture(t, "kb")

	// No frontmatter at all — ParseFact cannot build a fact from this.
	rec := f.do(http.MethodPut, f.factURL("kb/ai/test.md"), `{"content":"not a fact at all"}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if w := f.wrote(); len(w) != 0 {
		t.Errorf("the store wrote %v for unparseable content; the bad blob is now the branch HEAD for that path", w)
	}
}

// A prior-version read that fails (anything but "no such path") is a 500
// naming the store's error, and nothing is written. Sabotage: swallow the read
// error as "no prior version".
func TestHandleFactUpdate_PriorReadError_Returns500(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	f.seed(restWriteBranch, "kb/ai/test.md", factText(t, "kb/ai/test.md", "On X", 0.9, nil))
	injected := errors.New("w0h: injected prior read fault")
	var hits []string
	t.Cleanup(f.svc.Facts().SetReadFactFaultForTest(func(path string, _ *facts.ReadFactOpts) error {
		if path != "kb/ai/test.md" {
			return nil
		}
		hits = append(hits, path)
		return injected
	}))

	rec := f.do(http.MethodPut, f.factURL("kb/ai/test.md"), `{"content":"`+testFactContent+`"}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	p := writeProblem(t, rec)
	require.Equal(t, "Failed to read fact", p.Title)
	require.Contains(t, p.Detail, injected.Error())
	require.Equal(t, []string{"kb/ai/test.md"}, hits)
	require.Empty(t, f.wrote())
}
