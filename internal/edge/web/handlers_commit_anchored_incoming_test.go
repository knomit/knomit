package web

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/index"
)

// The commit-anchored /incoming and /outgoing read the fixture of
// handlers_fact_sub_test.go (newFactSubFixture). The fact read is a tripwire:
// these sub-resources are dispatched before it, so a test reaching it fails.

// anchoredServer is the fixture's server with the fact read as a tripwire:
// every ReadFact on the fixture's store fails (facts.Index.SetReadFactFaultForTest),
// and the test fails at cleanup if one was made.
func (f *factSubFixture) anchoredServer(t *testing.T) *Server {
	t.Helper()
	var hits atomic.Int64
	restore := f.svc.Facts().SetReadFactFaultForTest(func(string, *facts.ReadFactOpts) error {
		hits.Add(1)
		return errors.New("the fact read must not be reached")
	})
	t.Cleanup(func() {
		restore()
		if n := hits.Load(); n != 0 {
			t.Errorf("the fact read was reached %d times; the sub-resource is dispatched before it", n)
		}
	})
	return f.s
}

// anchoredURL is the commit-anchored sub-resource URL of path at step's
// commit on branchURL.
func (f *factSubFixture) anchoredURL(branchURL, step, path, sub string) string {
	return "/repos/alpha/branches/" + branchURL + "/commits/" + f.c[step] + "/facts/" + path + "/" + sub
}

// The commit-anchored /incoming is the lineage of the version live at the
// anchor, on the requested branch: at x0 that is kb/t.md v1 (p1, p2), at e14
// v2 (s1..s4, two retracted). The collection is pinned to the anchor, each
// item to its own commit. Sabotage: drop the anchor
// (v2 at x0), read the agent branch (s0 at x0), serve outgoing, link items at
// the anchor, or reverse them.
func TestCommitAnchoredIncoming_HappyPath(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)

	url := f.anchoredURL(fsOtherURL, "x0", "kb/t.md", "incoming")
	body := getRefs(t, s, url)
	require.Equal(t, APIBase+url, body.Links["self"].Href)
	f.requireRefs(t, body, fsOtherURL, inT1...)

	f.requireRefs(t, getRefs(t, s, f.anchoredURL(fsOtherURL, "e14", "kb/t.md", "incoming")), fsOtherURL, inT2...)

	// v1 read on the agent branch, at a1 (an A commit where kb/t.md is still
	// v1), keeps only that branch's source: p1 and p2 are E's.
	f.requireRefs(t, getRefs(t, s, f.anchoredURL("agent:test", "a1", "kb/t.md", "incoming")), "agent:test",
		wantRef{"kb/s0.md", "Agent source", "insight", "a1", false})
}

// The gate is evaluated at the ANCHOR, not at the branch's head. kb/r.md is
// live at x1 and retracted at E's head (e13), so without ?fallback the anchored
// edges at x1 are a 200 with r's lineage; with ?fallback=before at x0, which
// predates r, they are a 404 although r existed before E's head. Sabotage:
// check liveness at the branch head (x1 404s), or check existence at HEAD
// (x0 answers 200).
func TestCommitAnchoredSub_GateIsEvaluatedAtTheAnchor(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)
	reads := f.countGraphReads(t, "FactLiveAtCommit", "FactExistsAt", "IncomingAtCommit", "OutgoingAtCommit")

	in := getRefs(t, s, f.anchoredURL(fsOtherURL, "x1", "kb/r.md", "incoming"))
	f.requireRefs(t, in, fsOtherURL, inR...)
	out := getRefs(t, s, f.anchoredURL(fsOtherURL, "x1", "kb/r.md", "outgoing"))
	f.requireRefs(t, out, fsOtherURL, outO12...)
	require.EqualValues(t, 2, reads["FactLiveAtCommit"].Load())
	require.EqualValues(t, 0, reads["FactExistsAt"].Load())

	for _, sub := range []string{"incoming", "outgoing"} {
		f.requireAbsentAtCommit(t, s, f.anchoredURL(fsOtherURL, "x0", "kb/r.md", sub)+"?fallback=before", "kb/r.md", "x0")
	}
	require.EqualValues(t, 2, reads["FactExistsAt"].Load())
	require.EqualValues(t, 1, reads["IncomingAtCommit"].Load(), "only the x1 request reads the edges")
	require.EqualValues(t, 1, reads["OutgoingAtCommit"].Load(), "only the x1 request reads the edges")
}

// TestCommitAnchoredIncoming_UnknownRepo returns 404 problem+json.
func TestCommitAnchoredIncoming_UnknownRepo(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	rec := getAPI(t, s, "/repos/missing/branches/main/commits/abc123/facts/know/a.md/incoming")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// requireAbsentAtCommit requires the "no fact at path" 404 of the
// commit-anchored sub-resources.
func (f *factSubFixture) requireAbsentAtCommit(t *testing.T, s *Server, url, path, step string) {
	t.Helper()
	rec := getAPI(t, s, url)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	title, detail := decodeProblem(t, rec)
	require.Equal(t, "Fact not found", title)
	require.Equal(t, `no fact at path "`+path+`" on branch "agent/elsewhere" at commit "`+f.c[step]+`"`, detail)
}

// Without ?fallback=before the gate respects deletions: kb/r.md, retracted at
// e13, is gone at e14 (404, not an empty 200), checked with FactLiveAtCommit
// and never with the step-over FactExistsAt, and the edges are never read.
// Sabotage: gate on FactExistsAt (200 with q1, q2), or read the edges anyway.
func TestCommitAnchoredIncoming_FactNotLiveAtCommit(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)
	reads := f.countGraphReads(t, "FactLiveAtCommit", "FactExistsAt", "IncomingAtCommit", "OutgoingAtCommit")

	f.requireAbsentAtCommit(t, s, f.anchoredURL(fsOtherURL, "e14", "kb/r.md", "incoming"), "kb/r.md", "e14")
	require.EqualValues(t, 1, reads["FactLiveAtCommit"].Load())
	require.EqualValues(t, 0, reads["FactExistsAt"].Load())
	require.EqualValues(t, 0, reads["IncomingAtCommit"].Load(), "the edges of an absent fact must not be read")
	require.EqualValues(t, 0, reads["OutgoingAtCommit"].Load())
}

// TestCommitAnchoredOutgoing_FactNotLiveAtCommit mirrors the incoming case for
// the /outgoing sub-resource.
func TestCommitAnchoredOutgoing_FactNotLiveAtCommit(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)
	reads := f.countGraphReads(t, "FactLiveAtCommit", "FactExistsAt", "IncomingAtCommit", "OutgoingAtCommit")

	f.requireAbsentAtCommit(t, s, f.anchoredURL(fsOtherURL, "e14", "kb/r.md", "outgoing"), "kb/r.md", "e14")
	require.EqualValues(t, 1, reads["FactLiveAtCommit"].Load())
	require.EqualValues(t, 0, reads["FactExistsAt"].Load())
	require.EqualValues(t, 0, reads["OutgoingAtCommit"].Load(), "the edges of an absent fact must not be read")
	require.EqualValues(t, 0, reads["IncomingAtCommit"].Load())
}

// A failing liveness check is a 500 carrying the store's error, distinct from
// the 404 of a fact that is not live, and the edges are not read.
func TestCommitAnchoredIncoming_LivenessCheckError(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)
	reads := f.countGraphReads(t, "IncomingAtCommit")
	hits := f.failGraphRead(t, index.ReadFactLiveAtCommit, errors.New("liveness on fire"))

	rec := getAPI(t, s, f.anchoredURL(fsOtherURL, "x0", "kb/t.md", "incoming"))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	title, detail := decodeProblem(t, rec)
	require.Equal(t, "Failed to resolve fact", title)
	require.Equal(t, "liveness on fire", detail)
	require.EqualValues(t, 1, hits.Load())
	require.EqualValues(t, 0, reads["IncomingAtCommit"].Load())
}

// With ?fallback=before the gate is FactExistsAt (never FactLiveAtCommit), so
// kb/r.md, retracted at e13, still resolves at e14 to its last-valid version's
// edges: the sources q1, q2 citing it and the facts o1, o2 it cites. Sabotage:
// ignore ?fallback (404), or gate on FactLiveAtCommit.
func TestCommitAnchoredIncoming_FallbackBefore_RetractedStillResolves(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)
	reads := f.countGraphReads(t, "FactLiveAtCommit", "FactExistsAt")

	url := f.anchoredURL(fsOtherURL, "e14", "kb/r.md", "incoming") + "?fallback=before"
	body := getRefs(t, s, url)
	f.requireRefs(t, body, fsOtherURL, inR...)
	require.Equal(t, APIBase+f.anchoredURL(fsOtherURL, "e14", "kb/r.md", "incoming"), body.Links["self"].Href)

	out := getRefs(t, s, f.anchoredURL(fsOtherURL, "e14", "kb/r.md", "outgoing")+"?fallback=before")
	f.requireRefs(t, out, fsOtherURL, outO12...)

	require.EqualValues(t, 2, reads["FactExistsAt"].Load())
	require.EqualValues(t, 0, reads["FactLiveAtCommit"].Load())
}

// TestCommitAnchoredIncoming_FallbackBefore_NeverExisted_404: even with
// fallback, a fact that never existed in the ancestry 404s, and its edges are
// not read.
func TestCommitAnchoredIncoming_FallbackBefore_NeverExisted_404(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer(t)
	reads := f.countGraphReads(t, "FactExistsAt", "IncomingAtCommit")

	f.requireAbsentAtCommit(t, s,
		f.anchoredURL(fsOtherURL, "e14", "kb/never.md", "incoming")+"?fallback=before", "kb/never.md", "e14")
	require.EqualValues(t, 1, reads["FactExistsAt"].Load())
	require.EqualValues(t, 0, reads["IncomingAtCommit"].Load())
}
