package kb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
)

// The wiki-link rule on the REST writers: POST is a create (none in prose),
// PUT is judged as an edit against the stored body (none added).

func TestFactCreate_WikiLinkInProseRefused(t *testing.T) {
	_, ri, r := datedRepo(t)
	b := urlBranch(ri.AgentBranch())
	post := func(body string) *httptest.ResponseRecorder {
		js, err := json.Marshal(map[string]any{"title": "W", "body": body, "domain": []string{"technology", "wiki"}})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+b+"/facts", strings.NewReader(string(js)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := post("see [[f6db3a49]]")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "the body contains [[f6db3a49]] (line 1)")
	require.Contains(t, rec.Body.String(), "in refs")

	rec = post("code is fine: `[[oauth.client]]`")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestFactPut_WikiLinkEditRule(t *testing.T) {
	_, ri, r := datedRepo(t)
	b := urlBranch(ri.AgentBranch())
	const path = "kb/technology/dated/legacy.md"
	seedDatedFact(t, ri, path, "")
	put := func(p, body string) *httptest.ResponseRecorder {
		content := "---\ntype: hypothesis\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: []\n---\n# P\n\n" + body + "\n"
		js, err := json.Marshal(map[string]any{"content": content})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/repos/alpha/branches/"+b+"/facts/"+p, strings.NewReader(string(js)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := put(path, "now [[x]]")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "the seeded body has none, so one may not be added: "+rec.Body.String())
	require.Contains(t, rec.Body.String(), "the edit adds [[x]]")

	// Plant a legacy body with one, then keep it, then remove it.
	seedDatedFactBody(t, ri, path, "legacy [[a1b2c3d4]]")
	rec = put(path, "legacy, reworded [[a1b2c3d4]]")
	require.Equal(t, http.StatusOK, rec.Code, "keeping an existing one is allowed: "+rec.Body.String())
	rec = put(path, "legacy, cleaned")
	require.Equal(t, http.StatusOK, rec.Code, "removing it is allowed: "+rec.Body.String())

	rec = put("kb/technology/dated/fresh.md", "fresh [[y]]")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a PUT that creates is judged like a create: "+rec.Body.String())

	rec = put("artifacts/crawl/state.md", "saw [[link]]")
	require.Equal(t, http.StatusOK, rec.Code, "an artifact is exempt: "+rec.Body.String())
}

// seedDatedFactBody writes path with body straight to the store, as a fact
// written before the rule would have been.
func seedDatedFactBody(t *testing.T, ri *repos.RepoInstance, path, body string) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path,
			"---\ntype: hypothesis\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: []\n---\n# P\n\n"+body+"\n",
			"seed "+path, "")
		require.NoError(t, err)
	}))
}
