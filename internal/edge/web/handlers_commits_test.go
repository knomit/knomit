package web

import (
	"context"
	"encoding/json"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/index"
)

// The commits handlers read a REAL store: GET .../commits lists the branch's
// commit log under the ontology root, GET .../commits/{sha} serves one commit
// with its files' titles. Every choice the handlers make has two candidates
// in the fixture that give different answers, so swapping one for the other
// fails a test:
//
//   - the branch: the requested branch (agent/elsewhere) is never the repo's
//     agent branch (agent/test), and the two have diverged: kb/a.md has a
//     different current title on each, and kb/b.md exists only on elsewhere;
//   - the ontology root: commits and files outside kb/ exist (notes/...);
//   - the title source: the index's current title, the title at the commit
//     and the title before the commit all differ for the commits that use
//     them;
//   - the cursors: after, from and before each select a different page.

const (
	commitsAgentBranch = "agent/test"
	commitsOtherBranch = "agent/elsewhere"
	commitsOtherURL    = "agent:elsewhere"
	commitsRoot        = "kb"
)

// commitsFixture is the store the commits tests read, and the commit each
// step wrote.
type commitsFixture struct {
	s *Server
	// c1: agent/test adds kb/a.md "A one" (learn), BEFORE elsewhere forks.
	// c2: agent/test updates kb/a.md to "A agent two" (update).
	// c3: elsewhere updates kb/a.md to "A elsewhere two" (update).
	// c4: elsewhere adds kb/b.md "B one" (learn).
	// c5: elsewhere updates kb/b.md to "B two" AND adds kb/e.md "E one",
	//     one commit (update).
	// c6: elsewhere deletes kb/b.md AND kb/e.md, one commit (retract).
	// c7: elsewhere adds kb/c.md "C one", kb/d.md "D one" AND notes/out.md,
	//     one commit (learn).
	// c8: elsewhere adds only notes/only.md, outside the ontology root.
	c1, c2, c3, c4, c5, c6, c7, c8 string
}

func commitsFactText(title string) string {
	return "---\ntype: observation\nconfidence: 0.9\ndomain: [test]\n---\n# " + title + "\n\nbody\n"
}

func newCommitsFixture(t *testing.T) *commitsFixture {
	t.Helper()
	return buildCommitsFixture(t, false)
}

// newSpacedCommitsFixture is newCommitsFixture whose c5, c6 and c7 are each
// stamped in a later second than the one before, so the three carry distinct
// dates (committed_at has one-second resolution). It costs two seconds, so
// only the test that pins dates uses it.
func newSpacedCommitsFixture(t *testing.T) *commitsFixture {
	t.Helper()
	return buildCommitsFixture(t, true)
}

func buildCommitsFixture(t *testing.T, spaced bool) *commitsFixture {
	t.Helper()
	ctx := context.Background()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	f := &commitsFixture{s: &Server{Manager: m, OntologyRoot: commitsRoot}}

	write := func(branch, path, title, msg, op string) string {
		t.Helper()
		res, err := svc.Facts().WriteFact(ctx, branch, path, commitsFactText(title), msg, op)
		require.NoError(t, err)
		require.NotEmpty(t, res.CommitHash)
		return res.CommitHash
	}

	// The first line only is served; the second paragraph must not leak.
	f.c1 = write(commitsAgentBranch, "kb/a.md", "A one", "add a\n\nlonger description", "learn")
	require.NoError(t, svc.CreateBranch(ctx, commitsOtherBranch, commitsAgentBranch))
	f.c2 = write(commitsAgentBranch, "kb/a.md", "A agent two", "agent edits a", "update")
	f.c3 = write(commitsOtherBranch, "kb/a.md", "A elsewhere two", "elsewhere edits a", "update")
	f.c4 = write(commitsOtherBranch, "kb/b.md", "B one", "add b", "learn")
	c5, _, err := svc.Facts().BatchWriteFacts(ctx, commitsOtherBranch, map[string]string{
		"kb/b.md": commitsFactText("B two"),
		"kb/e.md": commitsFactText("E one"),
	}, nil, "edit b, add e", "update")
	require.NoError(t, err)
	f.c5 = c5
	if spaced {
		awaitNextSecond()
	}
	c6, _, err := svc.Facts().BatchWriteFacts(ctx, commitsOtherBranch, nil,
		[]string{"kb/b.md", "kb/e.md"}, "delete b and e", "retract")
	require.NoError(t, err)
	f.c6 = c6
	if spaced {
		awaitNextSecond()
	}
	c7, _, err := svc.Facts().BatchWriteFacts(ctx, commitsOtherBranch, map[string]string{
		"kb/c.md":      commitsFactText("C one"),
		"kb/d.md":      commitsFactText("D one"),
		"notes/out.md": commitsFactText("Out of root"),
	}, nil, "add c and a note", "learn")
	require.NoError(t, err)
	f.c7 = c7
	f.c8 = write(commitsOtherBranch, "notes/only.md", "Only a note", "add a note", "learn")
	return f
}

type commitsListBody struct {
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

func getCommitsList(t *testing.T, s *Server, url string) commitsListBody {
	t.Helper()
	rec := getAPI(t, s, url)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body commitsListBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

func (b commitsListBody) hashes() []string {
	out := make([]string, 0, len(b.Embedded.Commits))
	for _, c := range b.Embedded.Commits {
		out = append(out, c.Commit)
	}
	return out
}

// The list is the REQUESTED branch's log under the ontology root, newest
// first, one page of `limit`, with a next cursor at the page's last commit.
// Sabotage: list the agent branch (c2 appears, c3..c7 vanish), drop the
// ontology root (c8 appears), reverse the items, or swap next and prev.
func TestHandleCommitsList_ReturnsTheRequestedBranchUnderTheRoot(t *testing.T) {
	f := newSpacedCommitsFixture(t)
	base := httpx.APIBase + "/repos/alpha/branches/" + commitsOtherURL + "/commits"

	body := getCommitsList(t, f.s, "/repos/alpha/branches/"+commitsOtherURL+"/commits?limit=3")
	require.Equal(t, []string{f.c7, f.c6, f.c5}, body.hashes())
	require.Equal(t, 3, body.Count)
	require.Equal(t, base+"?limit=3", body.Links["self"].Href)
	require.Equal(t, base+"?after="+f.c5+"&limit=3", body.Links["next"].Href)
	require.NotContains(t, body.Links, "prev", "an `after` page has no prev cursor")

	// Each item carries the commit's own fields and a self link on the
	// requested branch.
	c7 := body.Embedded.Commits[0]
	require.Equal(t, "add c and a note", c7.Message)
	require.Equal(t, "learn", c7.Operation)
	require.Equal(t, index.FileCounts{Added: 3}, c7.Files, "counts are per commit, the note included")
	require.Equal(t, base+"/"+f.c7, c7.Links["self"].Href)
	requireRFC3339Date(t, c7.Date, "commit list item")
	// The store names a commit's author by the branch that wrote it and the
	// operation: each item carries its own.
	require.Equal(t, index.CommitAuthor{Name: "elsewhere", Email: "elsewhere+learn@agents.knomit.io"}, c7.Author)
	require.Equal(t, index.CommitAuthor{Name: "elsewhere", Email: "elsewhere+retract@agents.knomit.io"}, body.Embedded.Commits[1].Author)
	require.Equal(t, "retract", body.Embedded.Commits[1].Operation)
	require.Equal(t, index.FileCounts{Deleted: 2}, body.Embedded.Commits[1].Files)
	require.Equal(t, index.FileCounts{Added: 1, Modified: 1}, body.Embedded.Commits[2].Files)
	require.Equal(t, "update", body.Embedded.Commits[2].Operation)

	// c5, c6 and c7 were stamped in three different seconds, so each item
	// carries its own date, newest first, and the detail serves the same date
	// for the same commit. Sabotage: take every item's date from one entry,
	// or the detail's date from anywhere but its commit.
	d7, d6, d5 := c7.Date, body.Embedded.Commits[1].Date, body.Embedded.Commits[2].Date
	requireRFC3339Date(t, d6, "commit list item")
	requireRFC3339Date(t, d5, "commit list item")
	require.Greater(t, d7, d6, "c7 is stamped after c6")
	require.Greater(t, d6, d5, "c6 is stamped after c5")
	require.Equal(t, d7, getCommitDetail(t, f.s, commitsOtherURL, f.c7).Date)
	require.Equal(t, d5, getCommitDetail(t, f.s, commitsOtherURL, f.c5).Date)

	// Page two follows the next cursor and ends the log: c1 is shared with
	// the agent branch (written before the fork), c2 is not.
	page2 := getCommitsList(t, f.s, "/repos/alpha/branches/"+commitsOtherURL+"/commits?after="+f.c5+"&limit=3")
	require.Equal(t, []string{f.c4, f.c3, f.c1}, page2.hashes())
	require.NotContains(t, page2.Links, "next", "the last page has no next cursor")
	require.Equal(t, "add a", page2.Embedded.Commits[2].Message, "only the first line of the message")

	// The agent branch's own log, for contrast: same repo, other answer.
	agent := getCommitsList(t, f.s, "/repos/alpha/branches/agent:test/commits")
	require.Equal(t, []string{f.c2, f.c1}, agent.hashes())
}

// `from` and `before` reach the store as themselves: `from` starts a page AT
// the cursor (prev at its first item, next when more remain); `before` pages
// toward newer commits (prev when more remain). Each yields a page the other
// cursors do not. Sabotage: pass any cursor in another's place.
func TestHandleCommitsList_FromAndBeforeCursors(t *testing.T) {
	f := newCommitsFixture(t)
	base := httpx.APIBase + "/repos/alpha/branches/" + commitsOtherURL + "/commits"
	list := "/repos/alpha/branches/" + commitsOtherURL + "/commits"

	from := getCommitsList(t, f.s, list+"?from="+f.c5+"&limit=2")
	require.Equal(t, []string{f.c5, f.c4}, from.hashes())
	require.Equal(t, base+"?after="+f.c5+"&from="+f.c5+"&limit=2", from.Links["prev"].Href)
	require.Equal(t, base+"?after="+f.c4+"&from="+f.c5+"&limit=2", from.Links["next"].Href)

	before := getCommitsList(t, f.s, list+"?before="+f.c3+"&limit=2")
	// The store answers a `before` page from the newest end; more remain
	// between it and the cursor, so there is a prev link.
	require.Equal(t, []string{f.c7, f.c6}, before.hashes())
	require.Equal(t, base+"?after="+f.c7+"&before="+f.c3+"&limit=2", before.Links["prev"].Href)
	require.NotContains(t, before.Links, "next")
}

// A cursor naming no commit fails the store's log read, and the list reports
// that as a store error, not as an empty page.
func TestHandleCommitsList_UnknownCursorIsAStoreError(t *testing.T) {
	f := newCommitsFixture(t)
	sha := "0123456789abcdef0123456789abcdef01234567"

	rec := getAPI(t, f.s, "/repos/alpha/branches/"+commitsOtherURL+"/commits?after="+sha)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	title, detail := decodeProblem(t, rec)
	require.Equal(t, "Failed to list commits", title)
	require.Contains(t, detail, "LogPaginated: CommitLogQuery: after lookup:")
}

type commitDetailBody struct {
	Commit    string             `json:"commit"`
	Date      string             `json:"date"`
	Message   string             `json:"message"`
	Operation string             `json:"operation"`
	Author    index.CommitAuthor `json:"author"`
	Files     []struct {
		Path   string      `json:"path"`
		Action string      `json:"action"`
		Title  string      `json:"title"`
		Links  hal.LinkMap `json:"_links"`
	} `json:"files"`
	Links hal.LinkMap `json:"_links"`
}

func getCommitDetail(t *testing.T, s *Server, branchURL, sha string) commitDetailBody {
	t.Helper()
	rec := getAPI(t, s, "/repos/alpha/branches/"+branchURL+"/commits/"+sha)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body commitDetailBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// A file still indexed on the requested branch takes its CURRENT title there,
// not its title at the commit and not its title on the agent branch.
// Sabotage: skip the index (title "A one"), or read the agent branch (title
// "A agent two").
func TestHandleCommitDetail_LiveFileTakesTheRequestedBranchTitle(t *testing.T) {
	f := newCommitsFixture(t)
	branchURL := httpx.APIBase + "/repos/alpha/branches/" + commitsOtherURL

	body := getCommitDetail(t, f.s, commitsOtherURL, f.c1)
	require.Equal(t, f.c1, body.Commit)
	require.Equal(t, "add a", body.Message, "only the first line of the message")
	require.Equal(t, "learn", body.Operation)
	requireRFC3339Date(t, body.Date, "commit detail")
	// c1 was written by the agent branch, though it is read through elsewhere.
	require.Equal(t, index.CommitAuthor{Name: "test", Email: "test+learn@agents.knomit.io"}, body.Author)
	require.Len(t, body.Files, 1)
	require.Equal(t, "kb/a.md", body.Files[0].Path)
	require.Equal(t, "added", body.Files[0].Action)
	require.Equal(t, "A elsewhere two", body.Files[0].Title)
	require.Equal(t, branchURL+"/facts/kb/a.md", body.Files[0].Links["self"].Href)
	require.Equal(t, branchURL+"/commits/"+f.c1, body.Links["self"].Href)
	require.Equal(t, branchURL, body.Links["branch"].Href)
}

// A file no longer on the branch takes its title AT the commit when the
// commit has it: c5 modified kb/b.md to "B two" (the version before c5 is
// "B one") and added kb/e.md "E one"; both are gone at the branch tip, and
// each takes its own title. Sabotage: read before the commit instead (titles
// "B one" and ""), or read every file at the first file's path.
func TestHandleCommitDetail_GoneFileTakesItsTitleAtTheCommit(t *testing.T) {
	f := newCommitsFixture(t)

	body := getCommitDetail(t, f.s, commitsOtherURL, f.c5)
	require.Len(t, body.Files, 2, "%+v", body.Files)
	require.Equal(t, "kb/b.md", body.Files[0].Path)
	require.Equal(t, "modified", body.Files[0].Action)
	require.Equal(t, "B two", body.Files[0].Title)
	require.Equal(t, "kb/e.md", body.Files[1].Path)
	require.Equal(t, "added", body.Files[1].Action)
	require.Equal(t, "E one", body.Files[1].Title)
	require.Equal(t, "update", body.Operation)
}

// The commit that deleted a file has no version of it, so the file takes the
// title it had BEFORE the commit; c6 deleted two, and each takes its own.
// Sabotage: drop the last resort, read it at the commit (titles "" either
// way), or read every file at the first file's path. The branch passed to this read is not
// pinned: the store resolves "before" along the commit's first-parent chain
// and ignores the branch (vcs ResolveActiveCommitForPath), so a swapped
// branch is an equivalent mutant.
func TestHandleCommitDetail_DeletedFileTakesItsTitleBeforeTheCommit(t *testing.T) {
	f := newCommitsFixture(t)

	body := getCommitDetail(t, f.s, commitsOtherURL, f.c6)
	require.Len(t, body.Files, 2, "%+v", body.Files)
	require.Equal(t, "kb/b.md", body.Files[0].Path)
	require.Equal(t, "deleted", body.Files[0].Action)
	require.Equal(t, "B two", body.Files[0].Title)
	require.Equal(t, "kb/e.md", body.Files[1].Path)
	require.Equal(t, "deleted", body.Files[1].Action)
	require.Equal(t, "E one", body.Files[1].Title)
}

// Only files under the ontology root are listed (c7 also wrote notes/out.md),
// in path order, and each file carries ITS OWN title and link: c7 has two
// files under the root with different titles. Sabotage: pass no root (three
// files), reverse the files, or resolve every title from the first file.
func TestHandleCommitDetail_ListsOnlyFilesUnderTheRoot(t *testing.T) {
	f := newCommitsFixture(t)

	branchURL := httpx.APIBase + "/repos/alpha/branches/" + commitsOtherURL
	body := getCommitDetail(t, f.s, commitsOtherURL, f.c7)
	require.Len(t, body.Files, 2, "%+v", body.Files)
	for i, want := range []struct{ path, title string }{
		{"kb/c.md", "C one"},
		{"kb/d.md", "D one"},
	} {
		got := body.Files[i]
		require.Equal(t, want.path, got.Path, "file %d", i)
		require.Equal(t, "added", got.Action, "file %d", i)
		require.Equal(t, want.title, got.Title, "file %d", i)
		require.Equal(t, branchURL+"/facts/"+want.path, got.Links["self"].Href, "file %d", i)
	}
}

// An unknown commit is a 404 problem carrying the store's reason.
func TestHandleCommitDetail_UnknownCommitIs404(t *testing.T) {
	f := newCommitsFixture(t)
	sha := "0123456789abcdef0123456789abcdef01234567"

	rec := getAPI(t, f.s, "/repos/alpha/branches/"+commitsOtherURL+"/commits/"+sha)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	title, detail := decodeProblem(t, rec)
	require.Equal(t, "Commit not found", title)
	require.Equal(t, "CommitDetail: commit not found in history index: "+sha, detail)
}

// A repo with no live store answers the one store-unavailable 503 for both
// the commit list and a commit's detail: not an empty list that reads as "no
// commits", and not the detail's 404 "Commit not found" (user ruling
// 2026-10-24).
func TestHandleCommits_RepoWithoutAStore(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), OntologyRoot: commitsRoot}

	requireUnavailableProblem(t, getAPI(t, s, "/repos/alpha/branches/agent:test/commits"),
		"/repos/alpha/branches/agent:test/commits")
	const detail = "/repos/alpha/branches/agent:test/commits/0123456789abcdef0123456789abcdef01234567"
	requireUnavailableProblem(t, getAPI(t, s, detail), detail)
}

func TestHandleCommits_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/commits", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}
