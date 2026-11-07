package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// writeResolveFact writes one fact to the agent branch of svc.
func writeResolveFact(t *testing.T, svc *store.Service, path, typ, title, body string, confidence string) {
	t.Helper()
	kind := ""
	if typ == "policy" || typ == "heuristic" {
		kind = "kind: pragmatic\n"
	}
	_, err := svc.Facts().WriteFact(context.Background(), "agent/test", path,
		"---\n"+kind+"type: "+typ+"\nconfidence: "+confidence+"\nsources: 3\n---\n# "+title+"\n\n"+body+"\n",
		"add "+path, "")
	require.NoError(t, err)
}

// postResolve calls the resolver with the handles, as a caller whose reach is
// `reach` (nil: every repo).
func postResolve(t *testing.T, m *repos.Manager, reach func(string) bool, handles ...string) (int, []resolvedFact) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"handles": handles})
	require.NoError(t, err)
	return postResolveBody(t, m, reach, string(raw))
}

func postResolveBody(t *testing.T, m *repos.Manager, reach func(string) bool, body string) (int, []resolvedFact) {
	t.Helper()
	if reach == nil {
		reach = auth.AllRepos
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/facts/resolve", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithRepoReach(req.Context(), reach))
	rec := httptest.NewRecorder()
	handleFactsResolve(m, "kb")(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var out []resolvedFact
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return rec.Code, out
}

func handlesOf(rs []resolvedFact) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Handle
	}
	return out
}

// Every handle shape a chat answer cites resolves to the same fact: the short
// id, the path without root or extension, the full path, and the kb:// URI.
// Each result echoes the handle it answers and carries the fields the chip and
// the hover card draw.
func TestFactsResolve_EveryHandleShape(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	const path = "kb/invariants/web/gate/3fc77970.md"
	writeResolveFact(t, svcs["alpha"], path, "policy", "Agent config files are executable",
		"Files an agent loads on start change what it can do.\n\nReview them like code.", "0.92")
	id12 := fact.ID12(m.Get("alpha").ID())
	require.NotEmpty(t, id12, "fixture: the repo must have a root-commit id")

	handles := []string{
		"3fc77970",
		"invariants/web/gate/3fc77970",
		path,
		"kb://" + id12 + "/" + path,
		"kb://alpha/" + path,
	}
	code, got := postResolve(t, m, nil, handles...)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, handles, handlesOf(got), "one result per handle, in the order asked")
	for _, r := range got {
		require.Equal(t, "3fc77970", r.ID)
		require.Equal(t, "Agent config files are executable", r.Title)
		require.Equal(t, "policy", r.Type)
		require.Equal(t, "alpha", r.Repo)
		require.Equal(t, "agent/test", r.Branch)
		require.Equal(t, path, r.Path)
		require.NotNil(t, r.Confidence)
		require.InDelta(t, 0.92, *r.Confidence, 1e-9)
		// What Browse's FactMetaLine draws besides: kind, origin, sources.
		require.Equal(t, "pragmatic", r.Kind)
		require.Equal(t, "authored", r.Origin)
		require.Equal(t, 3, r.Sources)
		require.True(t, strings.HasPrefix(r.Excerpt, "Files an agent loads on start"), "excerpt %q", r.Excerpt)
		require.NotContains(t, r.Excerpt, "# Agent config", "the title heading is not part of the excerpt")
	}
}

// A fact the index lists but whose read fails (a body that no longer parses)
// still links by its index title and type, and carries NO confidence: absent,
// not 0, so the card's meta line leaves it out as Browse's does.
func TestFactsResolve_UnparsedFactHasNoConfidence(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	writeResolveFact(t, svcs["alpha"], "kb/a/b/99999999.md", "observation", "Broken policy", "b", "0.9")
	orig := readResolved
	t.Cleanup(func() { readResolved = orig })
	readResolved = func(context.Context, *repos.RepoInstance, hal.Anchor, string, bool) (fact.Fact, string, error) {
		return fact.Fact{}, "", errors.New(`ParseFact: invalid type "policy" for kind "epistemic"`)
	}
	raw, err := json.Marshal(map[string]any{"handles": []string{"99999999"}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/facts/resolve", bytes.NewBufferString(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithRepoReach(req.Context(), auth.AllRepos))
	rec := httptest.NewRecorder()
	handleFactsResolve(m, "kb")(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var items []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items), rec.Body.String())
	require.Len(t, items, 1, "fixture: the unparsed fact still resolves")
	require.Equal(t, "Broken policy", items[0]["title"])
	_, has := items[0]["confidence"]
	require.False(t, has, "an unparsed fact has no confidence: %s", rec.Body.String())
}

// Unknown handles are omitted, not errors: an 8-hex string that is no fact's
// id (a commit hash), a path with no fact, a kb:// URI naming no repo, a
// private path, and text that is no handle at all.
func TestFactsResolve_UnknownHandlesAreOmitted(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	writeResolveFact(t, svcs["alpha"], "kb/a/b/11111111.md", "observation", "Known", "body", "0.8")

	code, got := postResolve(t, m, nil,
		"deadbeef", "kb/a/b/22222222.md", "kb://000000000000/kb/a/b/11111111.md",
		"kb/.knomit/jobs/11111111.md", "not a handle", "11111111")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"11111111"}, handlesOf(got))
}

// The caller's read permission is the repo reach its credential records: a
// fact in a repo the caller cannot reach is omitted, by id, by path and by
// kb:// URI alike, while the same request resolves what it may read.
func TestFactsResolve_RespectsReadPermission(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha", "beta")
	writeResolveFact(t, svcs["alpha"], "kb/a/x/aaaaaaaa.md", "observation", "Alpha fact", "body a", "0.7")
	writeResolveFact(t, svcs["beta"], "kb/a/x/bbbbbbbb.md", "concept", "Beta fact", "body b", "0.7")
	betaURI := "kb://" + fact.ID12(m.Get("beta").ID()) + "/kb/a/x/bbbbbbbb.md"
	onlyAlpha := func(repo string) bool { return repo == "alpha" }

	code, got := postResolve(t, m, onlyAlpha, "aaaaaaaa", "bbbbbbbb", "kb/a/x/bbbbbbbb.md", betaURI)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"aaaaaaaa"}, handlesOf(got))
	require.Equal(t, "alpha", got[0].Repo)

	// The same handles, with both repos in reach, resolve across both.
	code, got = postResolve(t, m, nil, "aaaaaaaa", "bbbbbbbb", "kb/a/x/bbbbbbbb.md", betaURI)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"aaaaaaaa", "bbbbbbbb", "kb/a/x/bbbbbbbb.md", betaURI}, handlesOf(got))
	require.Equal(t, "beta", got[1].Repo)
	require.Equal(t, "concept", got[1].Type)
}

// One request resolves a whole turn's handles across repos; a repeated handle
// is answered once. Past the cap, and for a malformed body, the request is
// refused.
func TestFactsResolve_Batch(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha", "beta")
	writeResolveFact(t, svcs["alpha"], "kb/t/one/c0c0c0c0.md", "observation", "One", "b", "0.5")
	writeResolveFact(t, svcs["alpha"], "kb/t/two/c1c1c1c1.md", "pattern", "Two", "b", "0.5")
	writeResolveFact(t, svcs["beta"], "kb/t/three/c2c2c2c2.md", "insight", "Three", "b", "0.5")

	code, got := postResolve(t, m, nil, "c0c0c0c0", "c2c2c2c2", "c1c1c1c1", "c0c0c0c0")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"c0c0c0c0", "c2c2c2c2", "c1c1c1c1"}, handlesOf(got))
	require.Equal(t, []string{"One", "Three", "Two"}, []string{got[0].Title, got[1].Title, got[2].Title})

	code, got = postResolve(t, m, nil)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, got, "an empty batch is an empty array, not null")
	require.Empty(t, got)

	many := make([]string, maxResolveHandles+1)
	for i := range many {
		many[i] = "c0c0c0c0"
	}
	code, _ = postResolve(t, m, nil, many...)
	require.Equal(t, http.StatusBadRequest, code)

	code, _ = postResolveBody(t, m, nil, `{"handles": "c0c0c0c0"}`)
	require.Equal(t, http.StatusBadRequest, code)
}
