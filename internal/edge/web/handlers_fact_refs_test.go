package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The ref gate must hold on the REST write paths, not just on MCP.
//
// Before this, POST /facts and PUT /facts assigned the client's refs verbatim:
// the "a stored local fact ref resolves" invariant held for knomit_learn and
// was bypassable by pointing a different API at the same branch. Everything
// downstream — the edge builder, the provenance walk, the UI's fact/broken
// split — was already written assuming the state could not exist.
//
// Both handlers route through the same refs.Gate as knomit_learn, so these
// assert the shared behaviour reached them, not a second implementation of it.

func TestFactCreate_RejectsUnresolvableLocalRef(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"} // empty corpus: nothing resolves
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: writer},
	}

	body := `{"title":"T","body":"b","type":"observation","domain":["ai"],
	          "refs":["know/ai/ml/nope.md"]}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "know/ai/ml/nope.md") {
		t.Errorf("the problem detail must name the offending ref: %s", rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("a rejected create must never reach git, got %d writes", writer.writeCalls)
	}
}

// The positive control, so the test above is not passing because refs are
// rejected wholesale: a ref that DOES resolve is accepted.
func TestFactCreate_AcceptsResolvableLocalRef(t *testing.T) {
	writer := &stubFactWriter{
		writeHash: "abc123",
		existing:  map[string]bool{"know/ai/ml/target.md": true},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: writer},
	}

	body := `{"title":"T","body":"b","type":"observation","domain":["ai"],
	          "refs":["know/ai/ml/target.md"]}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 1 {
		t.Errorf("write calls: got %d, want 1", writer.writeCalls)
	}

	// The response view classifies the ref against the same resolver, so a ref
	// the gate accepted must not come back "broken".
	var view struct {
		Refs []RefView `json:"refs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(view.Refs) != 1 {
		t.Fatalf("refs: got %d, want 1", len(view.Refs))
	}
	if view.Refs[0].Path != "know/ai/ml/target.md" {
		t.Errorf("path: got %q, want the repo-relative path", view.Refs[0].Path)
	}
}

// PUT is gated for the same reason knomit_update is: refs replace wholesale, so
// a create-only gate is bypassed by writing a fact clean and then updating its
// refs to garbage.
func TestFactUpdate_RejectsUnresolvableLocalRef(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: writer},
	}

	content := "---\ntitle: T\ntype: observation\ndomain: [ai]\nconfidence: 0.8\nsources: 1\n" +
		"refs:\n  - know/ai/ml/nope.md\n---\n\n# T\n\nbody\n"
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/agent:test/facts/know/ai/ml/xyz99999.md", strings.NewReader(string(body))))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("a rejected update must never reach git, got %d writes", writer.writeCalls)
	}
}

// Writes are accepted on the agent branch and on this instance's own
// experiments only, so the write tests' request branch is an experiment.
const (
	writeRefsBranch = "exp/write-refs"
	writeRefsURL    = "exp:write-refs"
)

// newWriteRefsServer is a real store (no writer stub) where kb/eonly1.md and
// kb/eonly2.md exist on the experiment exp/write-refs (X) only, never on the
// agent branch agent/test, and kb/f.md exists on X to be updated.
func newWriteRefsServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	_, err := svc.Facts().WriteFact(ctx, fsAgentBranch, "kb/seed.md", factText(t, "kb/seed.md", "Seed", 0.9, nil), "seed", "learn")
	require.NoError(t, err)
	openRESTExperiment(t, svc, "write-refs")
	for _, p := range []string{"kb/eonly1.md", "kb/eonly2.md", "kb/f.md"} {
		_, err := svc.Facts().WriteFact(ctx, writeRefsBranch, p, factText(t, p, "On X "+p, 0.9, nil), "x: "+p, "learn")
		require.NoError(t, err)
	}
	return &Server{Manager: m, OntologyRoot: "kb"}
}

// requireWrittenRefKinds requires a write response's refs, in order, as
// repo-relative path → kind pairs (the write gate stores refs in the canonical
// kb://<id>/… form, so Raw is not the path the request sent).
func requireWrittenRefKinds(t *testing.T, rec *httptest.ResponseRecorder, want ...[2]string) {
	t.Helper()
	var view struct {
		Refs []RefView `json:"refs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view), rec.Body.String())
	got := make([][2]string, 0, len(view.Refs))
	for _, r := range view.Refs {
		got = append(got, [2]string{r.Path, r.Kind})
	}
	require.Equal(t, want, got)
}

// The create and update responses classify refs on the REQUEST branch at HEAD.
// Both refs exist on X only, so a resolver on the agent branch, or on no
// branch, calls them broken. Sabotage: build the response resolver with
// ri.AgentBranch() or "" in handleFactCreate / handleFactUpdate.
func TestFactWrite_ResponseRefsResolveOnTheRequestBranch(t *testing.T) {
	s := newWriteRefsServer(t)

	// (The gate rejects an unresolvable ref, so no write can carry a broken
	// one; the order differs between create and update so a resolver that
	// answered by position would be seen.)
	create := `{"title":"T","body":"b","type":"observation","domain":["arch","web"],
	            "refs":["kb/eonly1.md","kb/eonly2.md"]}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+writeRefsURL+"/facts", strings.NewReader(create)))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	requireWrittenRefKinds(t, rec, [2]string{"kb/eonly1.md", "fact"}, [2]string{"kb/eonly2.md", "fact"})

	content := "---\ntitle: T\ntype: observation\ndomain: [arch]\nconfidence: 0.8\nsources: 1\n" +
		"refs:\n  - kb/eonly2.md\n  - kb/eonly1.md\n---\n\n# T\n\nbody\n"
	body, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)
	rec = httptest.NewRecorder()
	req = fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/branches/"+writeRefsURL+"/facts/kb/f.md", strings.NewReader(string(body))))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireWrittenRefKinds(t, rec, [2]string{"kb/eonly2.md", "fact"}, [2]string{"kb/eonly1.md", "fact"})
}
