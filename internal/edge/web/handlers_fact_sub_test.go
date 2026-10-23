package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// The fact sub-resources (/commits, /incoming and /outgoing, at HEAD and at a
// commit) read a REAL store. The choices below each have two candidates in the
// fixtures that give different answers, so swapping one for the other fails a
// test. (Three reads ignore their branch argument, so the branch passed to
// FactLiveAtCommit, FactExistsAt and OutgoingAtCommit has no observable
// candidate; see the PR's sabotage notes.)
//
//   - the branch: requests read agent/elsewhere (E), never the repo's agent
//     branch agent/test (A). The two fork after g6. A then gets a source s0
//     citing kb/t.md and its own second version of kb/t.md; E gets other
//     sources and another second version; kb/gone.md is retracted on E only;
//   - the version (anchor commit): kb/t.md has v1 on both branches (refs o1,
//     o2; cited by p1, p2 on E) and v2 on E (refs o1, o2, o5, o6; cited by
//     s1..s4), so reading the wrong version returns another set;
//   - the fallback mode: kb/r.md is retracted at e13, so FactLiveAtCommit and
//     FactExistsAt disagree after it; kb/never.md never existed;
//   - the commit the gate checks (anchor vs branch HEAD): kb/r.md is live at
//     x1 but retracted at E's head, and does not exist yet at x0 although it
//     existed before E's head, so a gate evaluated at HEAD instead of the
//     anchor answers the other way at both anchors;
//   - incoming vs outgoing: the two sets of every version are disjoint;
//   - per item: every collection the handlers emit has at least two items on
//     each path of the item loop (live, and retracted with no self link), each
//     with its own path, title, type and commit, and on E no item's commit is
//     the request's anchor.
const (
	fsAgentBranch = "agent/test"
	fsOtherBranch = "agent/elsewhere"
	fsOtherURL    = "agent:elsewhere"
)

// factSubFixture is a real store, the server over it, and the commit each
// named step wrote.
type factSubFixture struct {
	s   *Server
	svc *store.Service
	c   map[string]string
}

// refsTo is a factText mod setting the fact's type and refs.
func refsTo(ty fact.Type, refs ...string) func(f *fact.Fact) {
	return func(f *fact.Fact) {
		f.Type = ty
		f.Refs = refs
	}
}

// newFactSubFixture builds the graph fixture. spaced stamps e5 and every later
// step in a later second than e4, so s1's committed_at differs from s2's; it
// costs a second, so only the test that pins committed_at uses it.
func newFactSubFixture(t *testing.T, spaced bool) *factSubFixture {
	t.Helper()
	ctx := context.Background()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	f := &factSubFixture{s: &Server{Manager: m}, svc: svc, c: map[string]string{}}

	write := func(step, branch, path, title, op string, mod func(*fact.Fact)) {
		t.Helper()
		res, err := svc.Facts().WriteFact(ctx, branch, path, factText(t, path, title, 0.9, mod), step+": "+path, op)
		require.NoError(t, err)
		require.NotEmpty(t, res.CommitHash)
		f.c[step] = res.CommitHash
	}
	retract := func(step, branch string, paths ...string) {
		t.Helper()
		h, _, err := svc.Facts().BatchWriteFacts(ctx, branch, nil, paths, step+": retract", "retract")
		require.NoError(t, err)
		f.c[step] = h
	}

	// Shared history, on A before the fork.
	write("g1", fsAgentBranch, "kb/o1.md", "Out one", "learn", refsTo(fact.Principle))
	write("g2", fsAgentBranch, "kb/o2.md", "Out two", "learn", refsTo(fact.Hypothesis))
	write("g3", fsAgentBranch, "kb/o3.md", "Out three", "learn", refsTo(fact.Concept))
	write("g5", fsAgentBranch, "kb/t.md", "Target one", "learn", refsTo(fact.Observation, "kb/o1.md", "kb/o2.md"))
	write("g6", fsAgentBranch, "kb/gone.md", "Gone on elsewhere", "learn", refsTo(fact.Observation, "kb/o1.md"))
	require.NoError(t, svc.CreateBranch(ctx, fsOtherBranch, fsAgentBranch))

	// A after the fork: the decoys a request on E must never see.
	write("a1", fsAgentBranch, "kb/s0.md", "Agent source", "learn", refsTo(fact.Insight, "kb/t.md"))
	write("a2", fsAgentBranch, "kb/t.md", "Target agent two", "update", refsTo(fact.Observation, "kb/o3.md"))
	write("a3", fsAgentBranch, "kb/gone.md", "Gone agent two", "update", refsTo(fact.Observation, "kb/o2.md"))

	// E: p1 and p2 cite v1; x0 is an anchor between them and v2 that touches
	// none of the facts the tests read.
	write("e1", fsOtherBranch, "kb/p1.md", "Early one", "learn", refsTo(fact.Reference, "kb/t.md"))
	write("e2", fsOtherBranch, "kb/p2.md", "Early two", "learn", refsTo(fact.Process, "kb/t.md"))
	write("x0", fsOtherBranch, "kb/x.md", "Unrelated", "learn", nil)
	// o5 and o6 exist on E only, so retracting them marks them deleted (o1 to
	// o3 stay live on A, and a graph node is shared by the branches).
	write("e2b", fsOtherBranch, "kb/o5.md", "Out five", "learn", refsTo(fact.Synthesis))
	write("e2c", fsOtherBranch, "kb/o6.md", "Out six", "learn", refsTo(fact.Methodology))
	write("e3", fsOtherBranch, "kb/t.md", "Target two", "update",
		refsTo(fact.Observation, "kb/o1.md", "kb/o2.md", "kb/o5.md", "kb/o6.md"))
	write("e4", fsOtherBranch, "kb/s1.md", "Source one", "learn", refsTo(fact.Principle, "kb/t.md"))
	if spaced {
		awaitNextSecond()
	}
	write("e5", fsOtherBranch, "kb/s2.md", "Source two", "learn", refsTo(fact.Hypothesis, "kb/t.md"))
	write("e6", fsOtherBranch, "kb/s3.md", "Source three", "learn", refsTo(fact.Synthesis, "kb/t.md"))
	write("e7", fsOtherBranch, "kb/s4.md", "Source four", "learn", refsTo(fact.Methodology, "kb/t.md"))
	retract("e8", fsOtherBranch, "kb/s3.md", "kb/s4.md", "kb/o5.md", "kb/o6.md")
	retract("e9", fsOtherBranch, "kb/gone.md")
	// r is cited by q1 and q2, then retracted: not live at E's head, but it
	// existed.
	write("e10", fsOtherBranch, "kb/r.md", "Retract me", "learn", refsTo(fact.Observation, "kb/o1.md", "kb/o2.md"))
	write("e11", fsOtherBranch, "kb/q1.md", "Cites r one", "learn", refsTo(fact.Concept, "kb/r.md"))
	write("e12", fsOtherBranch, "kb/q2.md", "Cites r two", "learn", refsTo(fact.Pattern, "kb/r.md"))
	// x1 is an anchor at which kb/r.md is still live; it touches nothing the
	// tests read.
	write("x1", fsOtherBranch, "kb/x.md", "Unrelated between", "update", nil)
	retract("e13", fsOtherBranch, "kb/r.md")
	write("e14", fsOtherBranch, "kb/x.md", "Unrelated two", "update", nil)
	return f
}

// wantRef is one expected graph item: step names the commit it is pinned to.
type wantRef struct {
	path, title, typ, step string
	deleted                bool
}

type refsBody struct {
	Count    int         `json:"count"`
	Links    hal.LinkMap `json:"_links"`
	Embedded struct {
		Refs []struct {
			Path        string      `json:"path"`
			Title       string      `json:"title"`
			Type        string      `json:"type"`
			Commit      string      `json:"commit"`
			CommittedAt int64       `json:"committed_at"`
			Deleted     bool        `json:"deleted"`
			Links       hal.LinkMap `json:"_links"`
		} `json:"refs"`
	} `json:"_embedded"`
}

// getRefs GETs a graph collection and requires a 200 HAL body.
func getRefs(t *testing.T, s *Server, url string) refsBody {
	t.Helper()
	rec := getAPI(t, s, url)
	return decodeRefs(t, rec)
}

func decodeRefs(t *testing.T, rec *httptest.ResponseRecorder) refsBody {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body refsBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// requireRefs checks body's items, in order, against want: each item's own
// path, title, type, commit and deleted flag, a committed_at, and a self link
// pinned to the item's OWN commit under branchPrefix (none for a retracted
// item). commits resolves want's step names.
func requireRefs(t *testing.T, body refsBody, commits map[string]string, branchPrefix string, want ...wantRef) {
	t.Helper()
	require.Equal(t, len(want), body.Count, "count")
	require.Len(t, body.Embedded.Refs, len(want))
	for i, w := range want {
		got := body.Embedded.Refs[i]
		commit := commits[w.step]
		require.NotEmpty(t, commit, "fixture step %s", w.step)
		require.Equal(t, w.path, got.Path, "item %d path", i)
		require.Equal(t, w.title, got.Title, "item %d title", i)
		require.Equal(t, w.typ, got.Type, "item %d type", i)
		require.Equal(t, commit, got.Commit, "item %d commit (%s)", i, w.step)
		require.Equal(t, w.deleted, got.Deleted, "item %d deleted", i)
		require.NotZero(t, got.CommittedAt, "item %d committed_at", i)
		if w.deleted {
			require.NotContains(t, got.Links, "self", "item %d: a retracted item has no self link", i)
			continue
		}
		require.Equal(t, branchPrefix+"/commits/"+commit+"/facts/"+w.path,
			got.Links["self"].Href, "item %d self link", i)
	}
}

// requireRefs is the package requireRefs over the fixture's repo alpha.
func (f *factSubFixture) requireRefs(t *testing.T, body refsBody, branchURL string, want ...wantRef) {
	t.Helper()
	requireRefs(t, body, f.c, APIBase+"/repos/alpha/branches/"+branchURL, want...)
}

// The fixture's graph sets, by version.
var (
	// incoming of kb/t.md v2 (E's head).
	inT2 = []wantRef{
		{"kb/s1.md", "Source one", "principle", "e4", false},
		{"kb/s2.md", "Source two", "hypothesis", "e5", false},
		{"kb/s3.md", "Source three", "synthesis", "e6", true},
		{"kb/s4.md", "Source four", "methodology", "e7", true},
	}
	// outgoing of kb/t.md v2.
	outT2 = []wantRef{
		{"kb/o1.md", "Out one", "principle", "g1", false},
		{"kb/o2.md", "Out two", "hypothesis", "g2", false},
		{"kb/o5.md", "Out five", "synthesis", "e2b", true},
		{"kb/o6.md", "Out six", "methodology", "e2c", true},
	}
	// incoming of kb/t.md v1 on E.
	inT1 = []wantRef{
		{"kb/p1.md", "Early one", "reference", "e1", false},
		{"kb/p2.md", "Early two", "process", "e2", false},
	}
	// outgoing of kb/t.md v1, and of kb/r.md.
	outO12 = []wantRef{
		{"kb/o1.md", "Out one", "principle", "g1", false},
		{"kb/o2.md", "Out two", "hypothesis", "g2", false},
	}
	// incoming of kb/r.md.
	inR = []wantRef{
		{"kb/q1.md", "Cites r one", "concept", "e11", false},
		{"kb/q2.md", "Cites r two", "pattern", "e12", false},
	}
)

// failGraphRead fails exactly the named index read of the fixture's store with
// err, counting the hits.
func (f *factSubFixture) failGraphRead(t *testing.T, read index.ReadName, err error) *atomic.Int64 {
	t.Helper()
	hits := &atomic.Int64{}
	t.Cleanup(f.svc.Search().SetReadFaultForTest(failIndexRead(read, err, hits)))
	return hits
}

// countGraphReads counts the fixture store's reads of the named methods
// through the index.Overrides observer (it never changes an answer).
func (f *factSubFixture) countGraphReads(t *testing.T, methods ...string) readCounts {
	t.Helper()
	rc := readCounts{}
	for _, meth := range methods {
		rc[meth] = &atomic.Int64{}
	}
	installMountOverrides(t, f.svc, "countGraphReads", index.Overrides{Reads: func(got string) error {
		if c, ok := rc[got]; ok {
			c.Add(1)
		}
		return nil
	}})
	return rc
}

// ── /incoming and /outgoing at HEAD ─────────────────────────────────────────

// HEAD /incoming is the requested branch's current version's lineage: every
// source citing kb/t.md v2 on E, each pinned to its own commit, retracted ones
// without a link. Sabotage: read the agent branch (no sources), serve the
// outgoing set, link an item at another item's commit, reverse the items, or
// take one item's committed_at for all.
func TestHandleFactIncoming_ReadsTheRequestedBranchAtHead(t *testing.T) {
	f := newFactSubFixture(t, true)
	url := "/repos/alpha/branches/" + fsOtherURL + "/facts/kb/t.md/incoming"

	body := getRefs(t, f.s, url)
	require.Equal(t, APIBase+url, body.Links["self"].Href)
	f.requireRefs(t, body, fsOtherURL, inT2...)
	// s1 was committed a second before s2: each item carries its own time.
	require.Less(t, body.Embedded.Refs[0].CommittedAt, body.Embedded.Refs[1].CommittedAt)

	// The agent branch's kb/t.md is another version, which nobody cites.
	agent := getRefs(t, f.s, "/repos/alpha/branches/agent:test/facts/kb/t.md/incoming")
	f.requireRefs(t, agent, "agent:test")
}

// HEAD /outgoing is what the requested branch's current version cites.
// Sabotage: read the agent branch (o3 only), or serve the incoming set.
func TestHandleFactOutgoing_ReadsTheRequestedBranchAtHead(t *testing.T) {
	f := newFactSubFixture(t, false)
	url := "/repos/alpha/branches/" + fsOtherURL + "/facts/kb/t.md/outgoing"

	body := getRefs(t, f.s, url)
	require.Equal(t, APIBase+url, body.Links["self"].Href)
	f.requireRefs(t, body, fsOtherURL, outT2...)

	agent := getRefs(t, f.s, "/repos/alpha/branches/agent:test/facts/kb/t.md/outgoing")
	f.requireRefs(t, agent, "agent:test", wantRef{"kb/o3.md", "Out three", "concept", "g3", false})
}

// A fact retracted at the branch's HEAD has no HEAD sub-resources: the store's
// ExplainFact answers ErrFactNotLive and the handlers map it to 404. kb/gone.md
// is retracted on E only, so the same request on A succeeds.
func TestHandleFactIncoming_FactNotLive_Returns404(t *testing.T) {
	f := newFactSubFixture(t, false)
	for _, sub := range []string{"incoming", "outgoing"} {
		rec := getAPI(t, f.s, "/repos/alpha/branches/"+fsOtherURL+"/facts/kb/gone.md/"+sub)
		require.Equal(t, http.StatusNotFound, rec.Code, "/%s: %s", sub, rec.Body.String())
		require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
		title, detail := decodeProblem(t, rec)
		require.Equal(t, "Fact not found", title, "/%s", sub)
		require.Contains(t, detail, `no live version of this fact on branch "agent/elsewhere"`, "/%s", sub)
	}

	agent := getRefs(t, f.s, "/repos/alpha/branches/agent:test/facts/kb/gone.md/outgoing")
	f.requireRefs(t, agent, "agent:test", wantRef{"kb/o2.md", "Out two", "hypothesis", "g2", false})
}

// A failing graph read is a 500 carrying the store's error, titled for the
// sub-resource that failed.
func TestHandleFactIncoming_StoreError_Returns500(t *testing.T) {
	for _, tc := range []struct{ sub, title string }{
		{"incoming", "Failed to load incoming refs"},
		{"outgoing", "Failed to load outgoing refs"},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			f := newFactSubFixture(t, false)
			hits := f.failGraphRead(t, index.ReadExplainFact, errors.New("explain on fire"))

			rec := getAPI(t, f.s, "/repos/alpha/branches/"+fsOtherURL+"/facts/kb/t.md/"+tc.sub)
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			title, detail := decodeProblem(t, rec)
			require.Equal(t, tc.title, title)
			require.Equal(t, "explain on fire", detail)
			require.EqualValues(t, 1, hits.Load())
		})
	}
}

func TestBuildGraphRefItems_AnchorsToSourceCommit(t *testing.T) {
	b := hal.URLBuilder{Base: "https://k.example.com"}
	a := hal.Anchor{Branch: "main"}
	refs := []index.RefSummary{
		{Path: "kb/d.md", Title: "D", Commit: "1234abc"},
		{Path: "kb/d.md", Title: "D v2", Commit: "1236def"},
	}

	got := buildGraphRefItems(b, "alpha", a, refs)
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	if got[0].Commit != "1234abc" {
		t.Errorf("got[0].Commit: %q, want %q", got[0].Commit, "1234abc")
	}
	if got[1].Commit != "1236def" {
		t.Errorf("got[1].Commit: %q, want %q", got[1].Commit, "1236def")
	}

	wantHref0 := "https://k.example.com/repos/alpha/branches/main/commits/1234abc/facts/kb/d.md"
	if got := got[0].Links["self"].Href; got != wantHref0 {
		t.Errorf("got[0] self: %q, want %q", got, wantHref0)
	}
	wantHref1 := "https://k.example.com/repos/alpha/branches/main/commits/1236def/facts/kb/d.md"
	if got := got[1].Links["self"].Href; got != wantHref1 {
		t.Errorf("got[1] self: %q, want %q", got, wantHref1)
	}
}

// ── /commits ────────────────────────────────────────────────────────────────

// factCommitsFixture is the store the per-fact commit log tests read.
type factCommitsFixture struct {
	s *Server
	// c1: A adds kb/a.md (learn), before E forks.
	// c2: A updates kb/a.md (update), after the fork.
	// c3: E updates kb/a.md (update).
	// c4: E adds kb/b.md only (learn).
	// c5: E updates kb/a.md AND adds kb/c.md, one commit (learn).
	// c6: E updates kb/a.md (update), a second after c5, so the two carry
	//     distinct dates (committed_at has one-second resolution).
	//
	// No page in these tests ENDS at c5. An `after` cursor at a commit that
	// touched several files repeats that commit on the next page of a
	// path-filtered log (the store compares the cursor's rowid over all of the
	// commit's files with the page's rowid over the path's files only). That is
	// existing store behaviour, out of scope here.
	c1, c2, c3, c4, c5, c6 string
}

func newFactCommitsFixture(t *testing.T) *factCommitsFixture {
	t.Helper()
	ctx := context.Background()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	f := &factCommitsFixture{s: &Server{Manager: m}}
	write := func(branch, path, title, msg, op string) string {
		t.Helper()
		res, err := svc.Facts().WriteFact(ctx, branch, path, factText(t, path, title, 0.9, nil), msg, op)
		require.NoError(t, err)
		return res.CommitHash
	}
	f.c1 = write(fsAgentBranch, "kb/a.md", "A one", "add a\n\nlonger description", "learn")
	require.NoError(t, svc.CreateBranch(ctx, fsOtherBranch, fsAgentBranch))
	f.c2 = write(fsAgentBranch, "kb/a.md", "A agent", "agent edits a", "update")
	f.c3 = write(fsOtherBranch, "kb/a.md", "A two", "elsewhere edits a", "update")
	f.c4 = write(fsOtherBranch, "kb/b.md", "B one", "add b", "learn")
	c5, _, err := svc.Facts().BatchWriteFacts(ctx, fsOtherBranch, map[string]string{
		"kb/a.md": factText(t, "kb/a.md", "A three", 0.9, nil),
		"kb/c.md": factText(t, "kb/c.md", "C one", 0.9, nil),
	}, nil, "edit a, add c", "learn")
	require.NoError(t, err)
	f.c5 = c5
	awaitNextSecond()
	f.c6 = write(fsOtherBranch, "kb/a.md", "A four", "edit a again", "update")
	return f
}

type factCommitsBody struct {
	Count    int         `json:"count"`
	Links    hal.LinkMap `json:"_links"`
	Embedded struct {
		Commits []struct {
			Commit    string             `json:"commit"`
			Date      string             `json:"date"`
			Message   string             `json:"message"`
			Operation string             `json:"operation"`
			Author    index.CommitAuthor `json:"author"`
			Files     index.FileCounts   `json:"files"`
			Links     hal.LinkMap        `json:"_links"`
		} `json:"commits"`
	} `json:"_embedded"`
}

func getFactCommits(t *testing.T, s *Server, url string) factCommitsBody {
	t.Helper()
	rec := getAPI(t, s, url)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body factCommitsBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

func (b factCommitsBody) hashes() []string {
	out := make([]string, 0, len(b.Embedded.Commits))
	for _, c := range b.Embedded.Commits {
		out = append(out, c.Commit)
	}
	return out
}

// The log is the REQUESTED branch's history of THIS fact, newest first, one
// page of `limit`, with a next cursor at the page's last commit; each item
// carries its own commit's fields and a self link on the requested branch.
// Sabotage: log the agent branch (c2 appears), log the whole branch (c4
// appears), reverse the items, take any field from one item for all, or swap
// next and prev.
func TestHandleFactCommits_ReturnsTheRequestedBranchsLogOfThePath(t *testing.T) {
	f := newFactCommitsFixture(t)
	list := "/repos/alpha/branches/" + fsOtherURL + "/facts/kb/a.md/commits"
	branch := APIBase + "/repos/alpha/branches/" + fsOtherURL

	body := getFactCommits(t, f.s, list+"?limit=3")
	require.Equal(t, []string{f.c6, f.c5, f.c3}, body.hashes())
	require.Equal(t, 3, body.Count)
	require.Equal(t, APIBase+list+"?limit=3", body.Links["self"].Href)
	require.Equal(t, APIBase+list+"?after="+f.c3+"&limit=3", body.Links["next"].Href)
	require.NotContains(t, body.Links, "prev", "an `after` page has no prev cursor")

	c6, c5, c3 := body.Embedded.Commits[0], body.Embedded.Commits[1], body.Embedded.Commits[2]
	require.Equal(t, "edit a again", c6.Message)
	require.Equal(t, "edit a, add c", c5.Message)
	require.Equal(t, "elsewhere edits a", c3.Message)
	require.Equal(t, "update", c6.Operation)
	require.Equal(t, "learn", c5.Operation)
	require.Equal(t, index.CommitAuthor{Name: "elsewhere", Email: "elsewhere+update@agents.knomit.io"}, c6.Author)
	require.Equal(t, index.CommitAuthor{Name: "elsewhere", Email: "elsewhere+learn@agents.knomit.io"}, c5.Author)
	require.Equal(t, index.FileCounts{Modified: 1}, c6.Files)
	require.Equal(t, index.FileCounts{Added: 1, Modified: 1}, c5.Files, "counts are per commit, kb/c.md included")
	require.Equal(t, branch+"/commits/"+f.c6, c6.Links["self"].Href)
	require.Equal(t, branch+"/commits/"+f.c5, c5.Links["self"].Href)
	require.Equal(t, branch+"/commits/"+f.c3, c3.Links["self"].Href)
	requireRFC3339Date(t, c6.Date, "fact commit")
	requireRFC3339Date(t, c5.Date, "fact commit")
	require.Greater(t, c6.Date, c5.Date, "c6 is stamped a second after c5")

	// Page two follows the next cursor and ends the log: c1 is shared with the
	// agent branch (written before the fork), c2 is not.
	page2 := getFactCommits(t, f.s, list+"?after="+f.c3+"&limit=3")
	require.Equal(t, []string{f.c1}, page2.hashes())
	require.NotContains(t, page2.Links, "next", "the last page has no next cursor")
	require.Equal(t, "add a", page2.Embedded.Commits[0].Message, "only the first line of the message")
	require.Equal(t, index.CommitAuthor{Name: "test", Email: "test+learn@agents.knomit.io"}, page2.Embedded.Commits[0].Author)

	// The agent branch's log of the same fact, for contrast.
	agent := getFactCommits(t, f.s, "/repos/alpha/branches/agent:test/facts/kb/a.md/commits")
	require.Equal(t, []string{f.c2, f.c1}, agent.hashes())
}

// `from` and `before` reach the store as themselves, each yielding a page the
// other cursors do not. Sabotage: pass any cursor in another's place.
func TestHandleFactCommits_FromAndBeforeCursors(t *testing.T) {
	f := newFactCommitsFixture(t)
	list := "/repos/alpha/branches/" + fsOtherURL + "/facts/kb/a.md/commits"

	from := getFactCommits(t, f.s, list+"?from="+f.c5+"&limit=2")
	require.Equal(t, []string{f.c5, f.c3}, from.hashes())
	require.Equal(t, APIBase+list+"?after="+f.c5+"&from="+f.c5+"&limit=2", from.Links["prev"].Href)
	require.Equal(t, APIBase+list+"?after="+f.c3+"&from="+f.c5+"&limit=2", from.Links["next"].Href)

	before := getFactCommits(t, f.s, list+"?before="+f.c1+"&limit=2")
	require.Equal(t, []string{f.c6, f.c5}, before.hashes())
	require.Equal(t, APIBase+list+"?after="+f.c6+"&before="+f.c1+"&limit=2", before.Links["prev"].Href)
	require.NotContains(t, before.Links, "next")
}

// A cursor naming no commit fails the store's log read; the handler reports it
// as a store error, not as an empty page.
func TestHandleFactCommits_UnknownCursorIsAStoreError(t *testing.T) {
	f := newFactCommitsFixture(t)
	sha := "0123456789abcdef0123456789abcdef01234567"

	rec := getAPI(t, f.s, "/repos/alpha/branches/"+fsOtherURL+"/facts/kb/a.md/commits?after="+sha)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	title, detail := decodeProblem(t, rec)
	require.Equal(t, "Failed to list commits", title)
	require.Contains(t, detail, "LogPaginated: CommitLogQuery: after lookup:")
}

func TestHandleFactCommits_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/missing/branches/main/facts/know/a.md/commits", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}
