package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	knomitfact "knomit/internal/domain/fact"
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

// The ref is on the AGENT branch only, never on the request branch, so a gate
// that resolved on ri.AgentBranch() would let it through. Sabotage: resolve
// refs on the agent branch, or answer true.
func TestFactCreate_RejectsUnresolvableLocalRef(t *testing.T) {
	f := newFactWriteFixture(t, "know")
	f.seed(fsAgentBranch, "know/ai/ml/nope.md", factText(t, "know/ai/ml/nope.md", "Agent only", 0.9, nil))

	rec := f.do(http.MethodPost, "/repos/alpha/branches/"+restWriteURL+"/facts",
		`{"title":"T","body":"b","type":"observation","domain":["ai"],"refs":["know/ai/ml/nope.md"]}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "know/ai/ml/nope.md") {
		t.Errorf("the problem detail must name the offending ref: %s", rec.Body.String())
	}
	if w := f.wrote(); len(w) != 0 {
		t.Errorf("a rejected create must never reach git, got writes %v", w)
	}
}

// The positive control, so the test above is not passing because refs are
// rejected wholesale: two refs that DO resolve on the request branch (and not
// on the agent branch) are accepted, stored canonical, and come back "fact".
func TestFactCreate_AcceptsResolvableLocalRef(t *testing.T) {
	f := newFactWriteFixture(t, "know")
	for _, p := range []string{"know/ai/ml/target.md", "know/ai/ml/second.md"} {
		f.seed(restWriteBranch, p, factText(t, p, "On X "+p, 0.9, nil))
	}

	rec := f.do(http.MethodPost, "/repos/alpha/branches/"+restWriteURL+"/facts",
		`{"title":"T","body":"b","type":"observation","domain":["ai"],"refs":["know/ai/ml/target.md","know/ai/ml/second.md"]}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	w := f.wrote()
	require.Len(t, w, 1)
	stored, ok := f.read(restWriteBranch, w[0])
	require.True(t, ok)
	id := knomitfact.ID12(f.s.Manager.Get("alpha").ID())
	require.Contains(t, stored, "kb://"+id+"/know/ai/ml/target.md", "the gate canonicalized the stored ref")

	// The response view classifies the refs against the request branch, so a
	// ref the gate accepted must not come back "broken".
	requireWrittenRefKinds(t, rec, [2]string{"know/ai/ml/target.md", "fact"}, [2]string{"know/ai/ml/second.md", "fact"})
}

// PUT is gated for the same reason knomit_update is: refs replace wholesale, so
// a create-only gate is bypassed by writing a fact clean and then updating its
// refs to garbage. The ref exists on the agent branch only.
func TestFactUpdate_RejectsUnresolvableLocalRef(t *testing.T) {
	f := newFactWriteFixture(t, "know")
	f.seed(fsAgentBranch, "know/ai/ml/nope.md", factText(t, "know/ai/ml/nope.md", "Agent only", 0.9, nil))

	content := "---\ntitle: T\ntype: observation\ndomain: [ai]\nconfidence: 0.8\nsources: 1\n" +
		"refs:\n  - know/ai/ml/nope.md\n---\n\n# T\n\nbody\n"
	body, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)

	rec := f.do(http.MethodPut, f.factURL("know/ai/ml/xyz99999.md"), string(body))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if w := f.wrote(); len(w) != 0 {
		t.Errorf("a rejected update must never reach git, got writes %v", w)
	}
}

// putRefs is a PUT body for kb/f.md carrying refs, in the order given.
func putRefs(t *testing.T, title string, refs ...string) string {
	t.Helper()
	content := "---\ntype: observation\ndomain: [arch]\nconfidence: 0.8\nsources: 1\nrefs:\n"
	for _, r := range refs {
		content += "  - " + r + "\n"
	}
	content += "---\n\n# " + title + "\n\nbody\n"
	body, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)
	return string(body)
}

// A PUT checks only the refs it ADDS: refs the stored version on the REQUEST
// branch already carried are not re-judged, even when they resolve nowhere
// (they resolved at their own commit). The agent branch's copy of the same
// path carries a different ref, so reading the prior version there flips both
// requests. Sabotage: read the prior version on ri.AgentBranch(), pass no
// prior, or pass the request's own refs as prior.
func TestFactUpdate_PriorRefsAreNotReJudged(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	withRefs := func(refs ...string) func(*knomitfact.Fact) {
		return func(x *knomitfact.Fact) { x.Refs = refs }
	}
	f.seed(restWriteBranch, "kb/f.md", factText(t, "kb/f.md", "On X", 0.9, withRefs("kb/ghost1.md", "kb/ghost2.md")))
	f.seed(fsAgentBranch, "kb/f.md", factText(t, "kb/f.md", "On A", 0.9, withRefs("kb/ghost3.md")))
	f.seed(restWriteBranch, "kb/real.md", factText(t, "kb/real.md", "Real", 0.9, nil))

	// Carried refs, in another order, plus one new ref that resolves: accepted.
	rec := f.do(http.MethodPut, f.factURL("kb/f.md"), putRefs(t, "Kept", "kb/ghost2.md", "kb/real.md", "kb/ghost1.md"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"kb/f.md"}, f.wrote())

	// A carried ref plus a NEW unresolvable one: refused, naming only the new.
	rec = f.do(http.MethodPut, f.factURL("kb/f.md"), putRefs(t, "Added", "kb/ghost1.md", "kb/ghost3.md"))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	p := writeProblem(t, rec)
	require.Contains(t, p.Detail, "kb/ghost3.md")
	require.NotContains(t, p.Detail, "kb/ghost1.md", "a carried ref is not re-judged")
	require.Equal(t, []string{"kb/f.md"}, f.wrote(), "the refused PUT wrote nothing")
}

// An unparseable stored version carries nothing forward: every ref of the PUT
// is checked. Sabotage: fail the PUT on the parse error instead (500).
func TestFactUpdate_UnparseablePriorExemptsNothing(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	f.seed(restWriteBranch, "kb/f.md", "not a fact, refs: [kb/ghost1.md]\n")

	rec := f.do(http.MethodPut, f.factURL("kb/f.md"), putRefs(t, "T", "kb/ghost1.md"))

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, writeProblem(t, rec).Detail, "kb/ghost1.md")
	require.Empty(t, f.wrote())
}

// Writes are accepted on the agent branch and on this instance's own
// experiments only, so the write tests' request branch is an experiment.
const (
	writeRefsBranch = "exp/write-refs"
	writeRefsURL    = "exp:write-refs"
)

// newWriteRefsServer is a real store where kb/eonly1.md and
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
	// one: this pins the branch the response resolves on, not the per-ref
	// classification, which TestBuildRefViews_EveryKindInOrder pins.)
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
