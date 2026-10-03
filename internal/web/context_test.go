package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

const webContextOntology = `id: t
name: T
topics:
  verdicts:
    description: verdicts
    context:
      task:    {type: string}
      verdict: {type: enum, values: [agree, disagree]}
      at:      {type: time}
  notes:
    description: notes
`

// contextManager creates repos with the context ontology (custom mode, so the
// declarations also survive Manager.Create's serialize-then-commit).
func contextManager(t *testing.T, names ...string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{Home: t.TempDir()}, AgentBranch: "machine/test", DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	for _, n := range names {
		_, err := m.Create(context.Background(), repos.CreateSpec{Name: n, Mode: "custom", OntologyYAML: webContextOntology}, nil)
		require.NoError(t, err)
	}
	return m
}

func seedVerdict(t *testing.T, ri *repos.RepoInstance, path, contextLine string) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path,
			"---\ntype: observation\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: []\n"+contextLine+"---\n# "+path+"\n\nbody\n",
			"add "+path, "")
		require.NoError(t, err)
	}))
}

type ctxRow struct {
	Path    string         `json:"path"`
	Context map[string]any `json:"context"`
}

func ctxRows(t *testing.T, rec *httptest.ResponseRecorder, key string) map[string]ctxRow {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var env struct {
		Embedded map[string][]ctxRow `json:"_embedded"`
		Facts    []ctxRow            `json:"facts"`
		Results  []ctxRow            `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	rows := env.Embedded[key]
	if rows == nil {
		rows = append(env.Facts, env.Results...)
	}
	out := map[string]ctxRow{}
	for _, r := range rows {
		out[r.Path[strings.LastIndex(r.Path, "/")+1:]] = r
	}
	return out
}

func ctxNames(rows map[string]ctxRow) []string {
	var out []string
	for n := range rows {
		out = append(out, strings.TrimSuffix(n, ".md"))
	}
	sort.Strings(out)
	return out
}

// C5, REST: every surface that takes the expiry params takes
// context.<key>=<value> too — repo facts, repo search, lens facts, lens search
// — with the same semantics, and carries the map on its rows. A bad key is 400.
func TestREST_ContextFilter(t *testing.T) {
	m := contextManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	seedVerdict(t, alpha, "kb/verdicts/a1.md", "context: {task: t-17, verdict: disagree}\n")
	seedVerdict(t, alpha, "kb/verdicts/a2.md", "context: {task: t-17, verdict: agree}\n")
	seedVerdict(t, alpha, "kb/verdicts/a3.md", "context: {task: t-18, verdict: disagree}\n")
	seedVerdict(t, beta, "kb/verdicts/b1.md", "context: {task: t-17, verdict: disagree}\n")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	b := urlBranch(alpha.AgentBranch())

	surfaces := []struct {
		name, base, key string
		want            []string
	}{
		{"facts collection", "/repos/alpha/branches/" + b + "/facts?path=kb/verdicts/", "facts", []string{"a1"}},
		{"search", "/repos/alpha/branches/" + b + "/search?path=kb/verdicts/", "results", []string{"a1"}},
		{"lens facts", "/lenses/eng/facts?path=kb/verdicts/", "facts", []string{"a1", "b1"}},
		{"lens search", "/lenses/eng/search?type=observation", "results", []string{"a1", "b1"}},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.base+"&context.task=t-17&context.verdict=disagree", nil))
			rows := ctxRows(t, rec, s.key)
			require.Equal(t, s.want, ctxNames(rows))
			for n, row := range rows {
				require.Equal(t, map[string]any{"task": "t-17", "verdict": "disagree"}, row.Context, n)
			}

			rec = httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.base+"&context.Task=t-17", nil))
			require.Equal(t, http.StatusBadRequest, rec.Code, "a bad key is a 400, not a silent empty result")
		})
	}

	rec, body := getExpiryJSON(t, r, "/repos/alpha/branches/"+b+"/facts/kb/verdicts/a2.md")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, map[string]any{"task": "t-17", "verdict": "agree"}, body["context"], "the single-fact view carries it")
}

// REST PUT is the one write path that commits the client's bytes, so it gets
// the gates explicitly: a malformed map (dropped by the lenient parser) is
// 422, an undeclared key is 422, and a time value is stored in UTC.
func TestREST_PutGatesContext(t *testing.T) {
	m := contextManager(t, "alpha")
	ri := m.Get("alpha")
	seedVerdict(t, ri, "kb/verdicts/a1.md", "")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	b := urlBranch(ri.AgentBranch())
	put := func(path, ctxLine string) *httptest.ResponseRecorder {
		content, err := json.Marshal(map[string]string{"content": "---\ntype: observation\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: []\n" + ctxLine + "---\n# P\n\nbody\n"})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/branches/"+b+"/facts/"+path, strings.NewReader(string(content))))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}
	stored := func(path string) string {
		var out string
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			f, err := svc.Facts().ReadFact(context.Background(), ri.AgentBranch(), path, nil)
			require.NoError(t, err)
			out = f.Content
		}))
		return out
	}

	rec := put("kb/verdicts/a1.md", "context: {task: \"a\\nb\"}\n")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Invalid context")
	require.NotContains(t, stored("kb/verdicts/a1.md"), "context", "nothing written")

	rec = put("kb/verdicts/a1.md", "context: {rogue: x}\n")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "not declared")

	rec = put("kb/notes/n1.md", "context: {task: t-1}\n")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "notes declares no context: %s", rec.Body.String())

	rec = put("kb/verdicts/a1.md", "context: {task: t-17, at: \"2026-10-01T02:00:00+02:00\"}\n")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, stored("kb/verdicts/a1.md"), "context: {at: \"2026-10-01T00:00:00Z\", task: t-17}\n", "a time is stored in UTC: the PUT was rewritten")
}

// C2, REST PUT with NO ontology (the bare test instance has none): a context
// is refused and nothing reaches git.
func TestREST_PutContextWithoutOntologyRefused(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), providers: storeProviders{factWriter: writer}}
	r := s.NewAPIRouter()
	body := `{"content":"---\ntype: observation\nconfidence: 0.9\nsources: 1\ncontext: {task: t-1}\n---\n# T\n\nBody.\n"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/branches/agent:test/facts/kb/ai/test.md", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "context is not allowed here")
	require.Equal(t, 0, writer.writeCalls, "nothing reached git")
}

// C7: POST create has no `context` field; the strict decoder refuses one, so
// a later field addition cannot skip the gates by accident.
func TestREST_PostRefusesContext(t *testing.T) {
	m := contextManager(t, "alpha")
	ri := m.Get("alpha")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+urlBranch(ri.AgentBranch())+"/facts",
		strings.NewReader(`{"title":"T","body":"b","domain":["verdicts","x"],"context":{"task":"t-1"}}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "context")
}
