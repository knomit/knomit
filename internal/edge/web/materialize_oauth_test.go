package web

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// newMaterializeRepos is a Manager holding alpha and beta, each with a real
// store on agent/test and the ontology root set.
func newMaterializeRepos(t *testing.T) (*repos.Manager, *repos.RepoInstance, *repos.RepoInstance) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	mk := func(name string) *repos.RepoInstance {
		svc, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
		ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
			Name: name, UID: "uid-" + name, AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
		})
		m.Set(name, ri)
		return ri
	}
	return m, mk("alpha"), mk("beta")
}

func writeObs(t *testing.T, ri *repos.RepoInstance, path, title string, refs ...string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title, f.Body, f.Type = title, title+" body.", fact.Observation
	f.Domain, f.Entities, f.Refs = []string{"testing"}, []string{}, refs
	f.Confidence, f.Sources = 0.9, 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteFact(context.Background(), "agent/test", path, content, "write "+path, "")
		require.NoError(t, werr)
	}))
}

// Cross-KB refs reach any repo registered on the server, but never further
// than the caller's own credential: a bearer token issued for alpha's MCP
// endpoint cannot read beta by its own routes, so materialize must not read
// beta for it either. The refusal reads EXACTLY like a repo that does not
// exist (kb_unavailable), so a caller cannot probe which repos exist. A token
// whose audience covers every repo follows the same ref.
func TestOAuthListener_MaterializeCrossKBStaysInsideTheTokensReach(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, beta := newMaterializeRepos(t)
	f.s.Manager = m
	beta12 := fact.ID12(beta.ID())
	xWire := "kb://" + beta12 + "/kb/b/x.md"
	zWire := "kb://0123456789ab/kb/z.md" // no repo has this id
	writeObs(t, beta, "kb/b/x.md", "X")
	writeObs(t, alpha, "kb/a/r.md", "R", xWire, zWire)

	materialize := func(tokenResource string) []map[string]any {
		t.Helper()
		tok := f.mint(t, "laptop-"+tokenResource, tokenResource, []string{"read"}, auth.Read)
		h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
		_, sid := rpcAt(t, h, alphaMCP, "", initBody)
		text, isErr := callToolAt(t, h, alphaMCP, sid, "knomit_materialize",
			`{"file":"kb/a/r.md","audience":"human","cross_kb":true}`)
		require.False(t, isErr, text)
		var page struct {
			Facts []map[string]any `json:"facts"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &page), text)
		return page.Facts
	}
	omitted := func(n map[string]any) []string {
		var out []string
		list, _ := n["not_included"].([]any)
		for _, o := range list {
			om := o.(map[string]any)
			out = append(out, om["path"].(string)+"|"+om["reason"].(string))
		}
		return out
	}

	confined := materialize(testIssuer + alphaMCP)
	require.Len(t, confined, 1, "beta is outside the token's reach: X is not read")
	require.Equal(t, []string{xWire + "|kb_unavailable", zWire + "|kb_unavailable"}, omitted(confined[0]),
		"an unreadable repo reads exactly like a repo that does not exist")

	open := materialize(testIssuer + "/api/v1")
	require.Len(t, open, 2, "a token for the whole API reaches beta")
	require.Equal(t, "X", open[1]["title"])
	require.Equal(t, []string{zWire + "|kb_unavailable"}, omitted(open[0]))
}
