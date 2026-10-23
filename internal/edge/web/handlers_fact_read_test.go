package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
)

// The fact reads (GET …/facts/{path} at HEAD and …/commits/{sha}/facts/{path})
// read a REAL store. Each choice readFact and the handlers make has two
// candidates in newFactReadFixture that give different answers:
//
//   - the branch of a HEAD read: requests read agent/elsewhere (E), never the
//     agent branch agent/test (A). kb/f.md has another version on A, A's head
//     is another commit, kb/gone.md is retracted on E only, and kb/aonly1.md
//     and kb/aonly2.md exist on A only, so a ref resolver on A calls them facts;
//   - the head stamped into a HEAD view: E's head (e10) touches none of the
//     facts read, so it is neither kb/f.md's own commit nor A's head;
//   - the version (anchor commit): kb/f.md v1 is live at x0, v2 at E's head;
//   - fallback or not: at e3, which wrote kb/f.md v2, the walk-back read
//     ("before e3") returns v1, so a read that always falls back is seen; at
//     e8 kb/r.md is retracted, so only the fallback finds it (v2, from e6),
//     and r is written again (v3) at e9b, so a walk-back from anywhere but
//     the pinned commit finds another version;
//   - the commit the ref resolver checks: every version read cites two facts
//     that exist at the view's commit and two that do not exist there yet but
//     do at a later candidate: kb/n2.md, kb/n3.md (after x0), kb/m1.md (after
//     e6, before e8) and kb/m2.md (after e8). So resolving refs at HEAD, at the
//     URL's sha instead of the fallback's commit, or once for every ref
//     (refs[0]) gives another list of kinds.
//
// (The branch passed to an anchored read and to an anchored FactExistsAt is
// discarded by the store, kb/gotchas/store/anchored-read-ignores-branch, so
// it has no observable candidate there.)
type factReadFixture struct {
	s   *Server
	svc *store.Service
	c   map[string]string
}

func newFactReadFixture(t *testing.T) *factReadFixture {
	t.Helper()
	ctx := context.Background()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	f := &factReadFixture{s: &Server{Manager: m}, svc: svc, c: map[string]string{}}

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
	plain := refsTo(fact.Observation)

	// Shared history, on A before the fork.
	write("g1", fsAgentBranch, "kb/n1.md", "Near one", "learn", plain)
	write("g2", fsAgentBranch, "kb/n0.md", "Near zero", "learn", plain)
	write("g3", fsAgentBranch, "kb/f.md", "Fact one", "learn",
		refsTo(fact.Observation, "kb/n1.md", "kb/n2.md", "kb/n0.md", "kb/n3.md"))
	write("g4", fsAgentBranch, "kb/gone.md", "Gone on elsewhere", "learn", plain)
	require.NoError(t, svc.CreateBranch(ctx, fsOtherBranch, fsAgentBranch))

	// A after the fork: what a request on E must never see.
	write("a1", fsAgentBranch, "kb/aonly1.md", "Agent only one", "learn", plain)
	write("a2", fsAgentBranch, "kb/aonly2.md", "Agent only two", "learn", plain)
	write("a3", fsAgentBranch, "kb/f.md", "Fact agent two", "update", plain)

	// E.
	write("x0", fsOtherBranch, "kb/x.md", "Unrelated", "learn", plain)
	write("e1", fsOtherBranch, "kb/n2.md", "Near two", "learn", plain)
	write("e2", fsOtherBranch, "kb/n3.md", "Near three", "learn", plain)
	write("e3", fsOtherBranch, "kb/f.md", "Fact two", "update",
		refsTo(fact.Observation, "kb/n1.md", "kb/aonly1.md", "kb/n0.md", "kb/aonly2.md"))
	retract("e4", fsOtherBranch, "kb/gone.md")
	write("e5", fsOtherBranch, "kb/r.md", "Retract one", "learn", plain)
	write("e6", fsOtherBranch, "kb/r.md", "Retract two", "update",
		refsTo(fact.Observation, "kb/n1.md", "kb/m1.md", "kb/n0.md", "kb/m2.md"))
	write("e7", fsOtherBranch, "kb/m1.md", "Mid one", "learn", plain)
	retract("e8", fsOtherBranch, "kb/r.md")
	write("e9", fsOtherBranch, "kb/m2.md", "Mid two", "learn", plain)
	write("e9b", fsOtherBranch, "kb/r.md", "Retract three", "learn", plain)
	write("e10", fsOtherBranch, "kb/x.md", "Unrelated two", "update", plain)
	return f
}

// factViewBody is the fact view's wire shape, as far as these tests read it.
type factViewBody struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Body  string `json:"body"`
	AsOf  AsOf   `json:"as_of"`
	Refs  []struct {
		Raw  string `json:"raw"`
		Kind string `json:"kind"`
	} `json:"refs"`
	Links hal.LinkMap `json:"_links"`
}

// getFactView GETs a fact view and requires a 200 HAL body.
func getFactView(t *testing.T, s *Server, url string) factViewBody {
	t.Helper()
	rec := getAPI(t, s, url)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body factViewBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// requireRefKinds requires the view's refs, in order, as raw path → kind pairs.
func requireRefKinds(t *testing.T, body factViewBody, want ...[2]string) {
	t.Helper()
	got := make([][2]string, 0, len(body.Refs))
	for _, r := range body.Refs {
		got = append(got, [2]string{r.Raw, r.Kind})
	}
	require.Equal(t, want, got, "refs (raw, kind)")
}

// requireProblem requires a problem+json answer with this status, title and
// detail.
func requireProblem(t *testing.T, s *Server, url string, status int, title, detail string) {
	t.Helper()
	rec := getAPI(t, s, url)
	require.Equal(t, status, rec.Code, rec.Body.String())
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	gotTitle, gotDetail := decodeProblem(t, rec)
	require.Equal(t, title, gotTitle)
	require.Equal(t, detail, gotDetail)
}

// readFactLog records every ReadFact a store serves as "head <path>",
// "at <sha> <path>" or "before <sha> <path>", and fails the read whose record
// equals fail (none when fail is ""). It goes through
// facts.Index.SetReadFactFaultForTest, which sees the read's opts, so it tells
// a pinned read from the walk-back one; a recorder is safe on several
// goroutines.
type readFactLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *readFactLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func hookReadFact(t *testing.T, svc *store.Service, fail string, err error) *readFactLog {
	t.Helper()
	l := &readFactLog{}
	t.Cleanup(svc.Facts().SetReadFactFaultForTest(func(path string, opts *facts.ReadFactOpts) error {
		rec := readFactRecord(path, opts)
		l.mu.Lock()
		l.calls = append(l.calls, rec)
		l.mu.Unlock()
		if fail != "" && rec == fail {
			return err
		}
		return nil
	}))
	return l
}

// readFactRecord is how readFactLog names one read.
func readFactRecord(path string, opts *facts.ReadFactOpts) string {
	switch {
	case opts.BeforeCommit != "":
		return "before " + opts.BeforeCommit + " " + path
	case opts.AtCommit != "":
		return "at " + opts.AtCommit + " " + path
	default:
		return "head " + path
	}
}

// at and before name the fixture's anchored reads of path at step.
func (f *factReadFixture) at(step, path string) string { return "at " + f.c[step] + " " + path }
func (f *factReadFixture) before(step, path string) string {
	return "before " + f.c[step] + " " + path
}

// ── HEAD ────────────────────────────────────────────────────────────────────

// A HEAD read is the requested branch's current version, stamped with that
// branch's head commit, and its refs are resolved on that branch at HEAD, one
// by one. Sabotage: read the agent branch ("Fact agent two"), stamp another
// head, resolve refs on the agent branch (the aonly refs become facts), or ask
// the resolver about refs[0] for every ref.
func TestHandleHALFact_ReadsTheRequestedBranchAtHead(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, "", nil)
	branch := APIBase + "/repos/alpha/branches/" + fsOtherURL

	body := getFactView(t, f.s, "/repos/alpha/branches/"+fsOtherURL+"/facts/kb/f.md")
	require.Equal(t, "kb/f.md", body.Path)
	require.Equal(t, "Fact two", body.Title)
	require.Equal(t, "cos=0.9", body.Body)
	require.Equal(t, fsOtherBranch, body.AsOf.Branch)
	require.Equal(t, f.c["e10"], body.AsOf.Commit, "as_of is E's head, not kb/f.md's own commit (e3) or A's head")
	require.Equal(t, map[string]string{
		"self":     branch + "/facts/kb/f.md",
		"incoming": branch + "/facts/kb/f.md/incoming",
		"outgoing": branch + "/facts/kb/f.md/outgoing",
		"commits":  branch + "/facts/kb/f.md/commits",
		"snapshot": branch + "/commits/" + f.c["e10"] + "/facts/kb/f.md",
		"parent":   branch + "/topics",
		"branch":   branch,
	}, hrefs(body.Links))
	requireRefKinds(t, body,
		[2]string{"kb/n1.md", "fact"}, [2]string{"kb/aonly1.md", "broken"},
		[2]string{"kb/n0.md", "fact"}, [2]string{"kb/aonly2.md", "broken"})
	require.Equal(t, []string{"head kb/f.md"}, reads.all())

	// The agent branch has its own version and head.
	agent := getFactView(t, f.s, "/repos/alpha/branches/agent:test/facts/kb/f.md")
	require.Equal(t, "Fact agent two", agent.Title)
	require.Equal(t, f.c["a3"], agent.AsOf.Commit)
}

// hrefs flattens a link map to rel → href.
func hrefs(l hal.LinkMap) map[string]string {
	out := map[string]string{}
	for rel, link := range l {
		out[rel] = link.Href
	}
	return out
}

// A fact with no live version on the requested branch is a 404 naming that
// branch: kb/gone.md is retracted on E only, and kb/missing.md never existed.
// Sabotage: read the agent branch (gone answers 200).
func TestHandleHALFact_NotFound_ReturnsProblem(t *testing.T) {
	f := newFactReadFixture(t)
	for _, path := range []string{"kb/gone.md", "kb/missing.md"} {
		requireProblem(t, f.s, "/repos/alpha/branches/"+fsOtherURL+"/facts/"+path, http.StatusNotFound,
			"Fact not found", `no fact at path "`+path+`" on branch "agent/elsewhere"`)
	}
	require.Equal(t, "Gone on elsewhere",
		getFactView(t, f.s, "/repos/alpha/branches/agent:test/facts/kb/gone.md").Title)
}

// A failing read is a 500 carrying the store's error, not a 404: real backend
// failures must not read as "no fact at this path".
func TestHandleHALFact_BackendError_Returns500(t *testing.T) {
	f := newFactReadFixture(t)
	reads := hookReadFact(t, f.svc, "head kb/f.md", errors.New("disk on fire"))

	requireProblem(t, f.s, "/repos/alpha/branches/"+fsOtherURL+"/facts/kb/f.md", http.StatusInternalServerError,
		"Failed to read fact", "disk on fire")
	require.Equal(t, []string{"head kb/f.md"}, reads.all())
}
