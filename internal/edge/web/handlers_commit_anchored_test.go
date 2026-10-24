package web

import (
	"errors"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The commit-anchored fact read runs on newFactReadFixture
// (handlers_fact_read_test.go).

// anchoredFactURL is the commit-anchored fact URL of path at step's commit on
// E.
func (f *factReadFixture) anchoredFactURL(step, path string) string {
	return "/repos/alpha/branches/" + fsOtherURL + "/commits/" + f.c[step] + "/facts/" + path
}

// requireAnchoredLinks requires the anchored view's links pinned to step's
// commit, with a live link to the HEAD view.
func (f *factReadFixture) requireAnchoredLinks(t *testing.T, body factViewBody, step, path string) {
	t.Helper()
	branch := httpx.APIBase + "/repos/alpha/branches/" + fsOtherURL
	got := hrefs(body.Links)
	require.ElementsMatch(t, []string{"self", "incoming", "outgoing", "commits", "live", "commit", "parent", "branch"},
		keys(got))
	require.Equal(t, branch+"/commits/"+f.c[step]+"/facts/"+path, got["self"])
	require.Equal(t, branch+"/commits/"+f.c[step]+"/facts/"+path+"/incoming", got["incoming"])
	require.Equal(t, branch+"/commits/"+f.c[step]+"/facts/"+path+"/outgoing", got["outgoing"])
	require.Equal(t, branch+"/commits/"+f.c[step], got["commit"])
	require.Equal(t, branch+"/facts/"+path, got["live"])
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The commit-anchored read is the version live at the pinned commit, stamped
// with that commit, its refs resolved at that commit. At x0 kb/f.md is v1,
// whose refs n2 and n3 are only written after x0. Sabotage: read HEAD ("Fact
// two"), stamp E's head, or resolve refs at HEAD (n2 and n3 become facts).
func TestHandleCommitAnchoredFact_ReturnsHALEnvelope(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, "", nil)

	body := getFactView(t, f.s, f.anchoredFactURL("x0", "kb/f.md"))
	require.Equal(t, "kb/f.md", body.Path)
	require.Equal(t, "Fact one", body.Title)
	require.Equal(t, fsOtherBranch, body.AsOf.Branch)
	require.Equal(t, f.c["x0"], body.AsOf.Commit, "as_of is the pinned commit, not kb/f.md's own commit (g3) or E's head")
	f.requireAnchoredLinks(t, body, "x0", "kb/f.md")
	requireRefKinds(t, body,
		[2]string{"kb/n1.md", "fact"}, [2]string{"kb/n2.md", "broken"},
		[2]string{"kb/n0.md", "fact"}, [2]string{"kb/n3.md", "broken"})
	require.Equal(t, []string{f.at("x0", "kb/f.md")}, reads.all())
}

// Without ?fallback=before a fact absent at the pinned commit is a 404 naming
// the commit, and the walk-back read never runs: kb/r.md is retracted at e8,
// kb/missing.md never existed. Sabotage: always fall back (r answers 200).
func TestHandleCommitAnchoredFact_NotFound(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, "", nil)

	for _, path := range []string{"kb/r.md", "kb/missing.md"} {
		requireProblem(t, f.s, f.anchoredFactURL("e8", path), http.StatusNotFound, "Fact not found",
			`no fact at path "`+path+`" on branch "agent/elsewhere" at commit "`+f.c["e8"]+`"`)
	}
	require.Equal(t, []string{f.at("e8", "kb/r.md"), f.at("e8", "kb/missing.md")}, reads.all())
}

// With ?fallback=before, a fact present at the pinned commit is read there, not
// walked back: e3 wrote kb/f.md v2, and the version before e3 is v1. Sabotage:
// always take the walk-back read ("Fact one", stamped g3).
func TestCommitAnchoredFact_FallbackBefore_FactExistsAtCommit(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, "", nil)

	body := getFactView(t, f.s, f.anchoredFactURL("e3", "kb/f.md")+"?fallback=before")
	require.Equal(t, "Fact two", body.Title)
	require.Equal(t, f.c["e3"], body.AsOf.Commit)
	f.requireAnchoredLinks(t, body, "e3", "kb/f.md")
	require.Equal(t, []string{f.at("e3", "kb/f.md")}, reads.all())
}

// With ?fallback=before, a fact retracted at the pinned commit resolves to its
// last valid version: kb/r.md, retracted at e8, is v2 from e6, and the view —
// as_of, links and ref resolution — is anchored at e6, not at the URL's e8.
// kb/m1.md is written between them and kb/m2.md after e8. Sabotage: ignore
// ?fallback (404), stamp or link the URL's sha, resolve refs at e8 (m1 becomes
// a fact) or at HEAD (m1 and m2 do).
func TestCommitAnchoredFact_FallbackBefore_FactRetractedAtCommit(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, "", nil)

	body := getFactView(t, f.s, f.anchoredFactURL("e8", "kb/r.md")+"?fallback=before")
	require.Equal(t, "Retract two", body.Title)
	require.Equal(t, fsOtherBranch, body.AsOf.Branch)
	require.Equal(t, f.c["e6"], body.AsOf.Commit, "as_of is the version's own commit, not the URL's")
	f.requireAnchoredLinks(t, body, "e6", "kb/r.md")
	requireRefKinds(t, body,
		[2]string{"kb/n1.md", "fact"}, [2]string{"kb/m1.md", "broken"},
		[2]string{"kb/n0.md", "fact"}, [2]string{"kb/m2.md", "broken"})
	require.Equal(t, []string{f.at("e8", "kb/r.md"), f.before("e8", "kb/r.md")}, reads.all())
}

// TestCommitAnchoredFact_FallbackBefore_StillNotFound: a fact that never
// existed in the ancestry is a 404 even with the fallback, which was tried.
func TestCommitAnchoredFact_FallbackBefore_StillNotFound(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, "", nil)

	requireProblem(t, f.s, f.anchoredFactURL("e8", "kb/never.md")+"?fallback=before", http.StatusNotFound,
		"Fact not found", `no fact at path "kb/never.md" on branch "agent/elsewhere" at commit "`+f.c["e8"]+`"`)
	require.Equal(t, []string{f.at("e8", "kb/never.md"), f.before("e8", "kb/never.md")}, reads.all())
}

// A pinned read that fails with anything but "not found" is a 500 carrying the
// store's error, and does not fall back. Sabotage: fall back on every error
// (200 "Retract two"), or map every error to 404.
func TestCommitAnchoredFact_ReadError_Returns500WithoutFallback(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, f.at("e8", "kb/r.md"), errors.New("pinned read on fire"))

	requireProblem(t, f.s, f.anchoredFactURL("e8", "kb/r.md")+"?fallback=before", http.StatusInternalServerError,
		"Failed to read fact", "pinned read on fire")
	require.Equal(t, []string{f.at("e8", "kb/r.md")}, reads.all())
}

// A walk-back read that fails with anything but "not found" is a 500 carrying
// the store's error. Sabotage: map the fallback's error to 404.
func TestCommitAnchoredFact_FallbackError_Returns500(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, f.before("e8", "kb/r.md"), errors.New("walk-back on fire"))

	requireProblem(t, f.s, f.anchoredFactURL("e8", "kb/r.md")+"?fallback=before", http.StatusInternalServerError,
		"Failed to read fact", "walk-back on fire")
	require.Equal(t, []string{f.at("e8", "kb/r.md"), f.before("e8", "kb/r.md")}, reads.all())
}

// The commit-anchored /outgoing is what the version live at the anchor cites
// (newFactSubFixture): at x0 kb/t.md v1 cites o1, o2; at e14 v2 cites o1, o2
// and the retracted o5, o6. The collection is pinned to the anchor, each item
// to its target's own commit. The fact read is a tripwire: the outgoing
// dispatch happens before it. Sabotage: drop the anchor (v2 at x0), serve
// incoming, link items at the anchor, or reverse them.
func TestHandleCommitAnchoredOutgoing_ReturnsCollection(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)

	url := f.anchoredURL(fsOtherURL, "x0", "kb/t.md", "outgoing")
	body := getRefs(t, s, url)
	if got, want := body.Links["self"].Href, httpx.APIBase+url; got != want {
		t.Errorf("self: got %q, want %q", got, want)
	}
	f.requireRefs(t, body, fsOtherURL, outO12...)

	f.requireRefs(t, getRefs(t, s, f.anchoredURL(fsOtherURL, "e14", "kb/t.md", "outgoing")), fsOtherURL, outT2...)
}

// TestHandleCommitAnchoredOutgoing_UnknownRepo returns 404 problem+json.
func TestHandleCommitAnchoredOutgoing_UnknownRepo(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/missing/branches/main/commits/abc123/facts/know/a.md/outgoing", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}
