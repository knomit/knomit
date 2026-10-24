package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// restPrinciple renders a principles fact; designer adds the entity the code
// preset's must-have-designer-entity rule requires.
func restPrinciple(t *testing.T, body string, designer bool) string {
	t.Helper()
	f := knomitfact.NewFact("placeholder.md")
	f.Title, f.Body = "A principle", body
	f.Kind, f.Type = knomitfact.Pragmatic, knomitfact.Policy
	f.Confidence, f.Sources, f.Domain = 0.9, 1, []string{"global"}
	if designer {
		f.Entities = []string{"designer"}
	}
	out, err := knomitfact.SerializeFact(f)
	require.NoError(t, err)
	return out
}

// RuleValidation on the REST fact writes (user ruling 2026-10-16): PUT and
// POST commit through the store, whose write cores validate every fact
// against the ontology at the branch's tip, so a write that fails a rule is
// refused at the source with 422 and the ontology's own message, and nothing
// is committed. Editing a legacy invalid principle (one the repository held
// before any rule ran) without fixing it is refused the same way; fixing it
// is accepted. The repository is on the code preset.
//
// SABOTAGE: drop the FactInvalidError arm from httpx.WriteStoreError -> the three
// refusals answer 500 -> red; drop validateWrite from writeFileExact -> they
// answer 200/201 and land -> red.
func TestFactWrite_RESTRefusesAFactThatFailsTheOntology(t *testing.T) {
	ctx := context.Background()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	ontYAML, err := knomitfact.CodeOntology().Serialize()
	require.NoError(t, err)
	const legacy = "kb/principles/ui/legacy.md"
	require.NoError(t, svc.InitRepo(ctx, map[string]string{
		knomitfact.OntologyFile: string(ontYAML),
		legacy:                  restPrinciple(t, "legacy body", false),
	}, "agent/test"))
	m := repos.New(ctx, repos.Deps{})
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb", Ontology: knomitfact.CodeOntology(),
	}))
	s := &Server{Manager: m, OntologyRoot: "kb"}
	r := s.NewAPIRouter()

	put := func(p, content string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{"content": content})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/branches/agent:test/facts/"+p, strings.NewReader(string(body))))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}
	tip := func() string {
		h, err := svc.HeadCommit(ctx, "agent/test")
		require.NoError(t, err)
		return h
	}
	refused := func(rec *httptest.ResponseRecorder, before string) {
		t.Helper()
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "must-have-designer-entity")
		require.Contains(t, rec.Body.String(), "principles must be authored via /knomit-principle")
		require.Equal(t, before, tip(), "nothing was committed")
	}

	before := tip()
	refused(put("kb/principles/ux/new.md", restPrinciple(t, "new", false)), before)
	refused(put(legacy, restPrinciple(t, "edited, still no designer", false)), before)

	// POST (create) goes through the same writer; its path is
	// kb/<domain[0]>/<domain[1]>/<id>.md, here under principles/.
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts",
		strings.NewReader(`{"title":"A principle","body":"b","kind":"pragmatic","type":"policy","domain":["principles","ux"],"confidence":0.9,"sources":1}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	refused(rec, before)

	rec = put(legacy, restPrinciple(t, "edited and fixed", true))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotEqual(t, before, tip(), "the fixed edit landed")
}
