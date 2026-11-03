package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// Placement on the REST fact writes (user ruling 2026-11-03: "the first level
// MUST always match the ontology, LLMs cannot just willy nilly invent
// topics"). A REST write that CREATES a fact — POST, or PUT to a fresh path —
// must name one of the repository's ontology topics, by the same check and
// with the same error text as knomit_learn, because a lens read skips a member
// whose ontology lacks the topic a path names (internal/corpus/federate), so
// a fact under an invented topic would be invisible through every lens.
//
// The repo is on the code preset (topics architecture, conventions, ...), and
// holds one legacy fact under a topic the preset lacks.

const (
	placementBranch = "agent/test"
	placementLegacy = "kb/legacy/old/l1.md"
)

type placementFixture struct {
	h   http.Handler
	m   *repos.Manager
	svc *store.Service
}

func newPlacementFixture(t *testing.T) placementFixture {
	t.Helper()
	ctx := context.Background()
	m := repos.New(ctx, repos.Deps{
		Cfg:     config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID(placementBranch), AgentBranch: placementBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	svc, err := store.Open(filepath.Join(t.TempDir(), "alpha.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(ctx, map[string]string{
		placementLegacy: placementFact("Legacy fact", "legacy body"),
	}, placementBranch))
	require.NoError(t, m.Repos().Insert(control.RepoRecord{
		UID: "uid-alpha", Name: "alpha", State: control.StateActive, Profile: "code", CreatedAt: 1,
	}))
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", Svc: svc, AgentBranch: placementBranch,
		Ontology: knomitfact.CodeOntology(), OntologyRoot: "kb",
	}))

	st := newClientSessionsStore(t)
	m.SetClientSessions(st)
	s := &Server{Manager: m, ClientSessions: st, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return placementFixture{h: s.NewAPIRouter(), m: m, svc: svc}
}

func placementFact(title, body string) string {
	f := knomitfact.NewFact("placeholder.md")
	f.Title, f.Body = title, body
	f.Kind, f.Type = knomitfact.Epistemic, knomitfact.Observation
	f.Confidence, f.Sources, f.Domain = 0.8, 1, []string{"placement"}
	out, err := knomitfact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

func (fx placementFixture) serve(t *testing.T, method, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(method, url, strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	fx.h.ServeHTTP(rec, req)
	return rec
}

func (fx placementFixture) post(t *testing.T, domain string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.serve(t, http.MethodPost, "/repos/alpha/branches/agent:test/facts",
		fmt.Sprintf(`{"title":"A fact","body":"b","domain":%s}`, domain))
}

func (fx placementFixture) put(t *testing.T, path, content string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)
	return fx.serve(t, http.MethodPut, "/repos/alpha/branches/agent:test/facts/"+path, string(body))
}

func (fx placementFixture) tip(t *testing.T) string {
	t.Helper()
	h, err := fx.svc.HeadCommit(context.Background(), placementBranch)
	require.NoError(t, err)
	return h
}

func (fx placementFixture) paths(t *testing.T) []string {
	t.Helper()
	got, err := fx.svc.Facts().ListAll(context.Background(), placementBranch)
	require.NoError(t, err)
	return got
}

// refused asserts a 422 placement problem carrying want, and that git did not
// move: same branch tip, same set of files.
func (fx placementFixture) refused(t *testing.T, rec *httptest.ResponseRecorder, want, tipBefore string, pathsBefore []string) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	require.Equal(t, "Invalid fact placement", p.Title, rec.Body.String())
	require.Contains(t, p.Detail, want)
	require.Equal(t, tipBefore, fx.tip(t), "nothing was committed")
	require.ElementsMatch(t, pathsBefore, fx.paths(t), "no file was written")
}

// POST /facts: an unknown topic, and a blank category, are 422 and write
// nothing; a topic the ontology declares is 201 under that topic.
//
// SABOTAGE: drop the ValidatePlacement call from handleFactCreate -> the
// refusals answer 201 and land a file -> red.
func TestFactCreate_RefusesAnUnknownTopic(t *testing.T) {
	fx := newPlacementFixture(t)
	tip, paths := fx.tip(t), fx.paths(t)

	fx.refused(t, fx.post(t, `["chain","links"]`), `unknown topic "chain"`, tip, paths)
	// No domain: the handler's fallback topic "uncategorized" is not a topic
	// of this ontology either.
	fx.refused(t, fx.post(t, `[]`), `unknown topic "uncategorized"`, tip, paths)
	fx.refused(t, fx.post(t, `["architecture","  "]`), "category is required", tip, paths)

	rec := fx.post(t, `["architecture","chain"]`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NotEqual(t, tip, fx.tip(t), "the valid create landed")
	var created []string
	for _, p := range fx.paths(t) {
		if strings.HasPrefix(p, "kb/architecture/chain/") {
			created = append(created, p)
		}
	}
	require.Len(t, created, 1, "the valid create is the one new fact: %v", fx.paths(t))
}

// PUT /facts/{path}: a PUT to a FRESH path creates a fact, so an unknown
// topic is 422 and writes nothing, and a declared one is 200. A PUT to an
// EXISTING fact under a topic the ontology lacks is an edit and is not
// re-judged (knomit_update does not re-judge it either). An artifact has no
// ontology placement.
//
// SABOTAGE: drop the !exists placement block from handleFactUpdate -> the
// fresh unknown-topic PUT answers 200 and lands -> red. Drop the !exists
// condition (judge every PUT) -> the legacy edit answers 422 -> red.
func TestFactUpdate_FreshPathRefusesAnUnknownTopic(t *testing.T) {
	fx := newPlacementFixture(t)
	tip, paths := fx.tip(t), fx.paths(t)

	fx.refused(t, fx.put(t, "kb/chain/links/p1.md", placementFact("Fresh", "b")), `unknown topic "chain"`, tip, paths)
	// Mixed case: the store lowercases the path, the topic check does too.
	fx.refused(t, fx.put(t, "KB/Chain/links/p2.md", placementFact("Fresh", "b")), `unknown topic "chain"`, tip, paths)

	rec := fx.put(t, "kb/architecture/chain/p3.md", placementFact("Fresh valid", "b"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, fx.paths(t), "kb/architecture/chain/p3.md")

	before := fx.tip(t)
	rec = fx.put(t, placementLegacy, placementFact("Legacy fact", "edited body"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotEqual(t, before, fx.tip(t), "the legacy edit landed")

	rec = fx.put(t, "artifacts/jobs/state.md", placementFact("Job state", "b"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// REST and MCP refuse the same input with the same text: one topic/category
// pair, sent to POST /facts and to knomit_learn on the repo's MCP mount.
//
// SABOTAGE: drop the ValidatePlacement call from handleFactCreate -> REST
// answers 201 while MCP refuses -> red.
func TestFactPlacement_RESTAndMCPRefuseTheSameInput(t *testing.T) {
	fx := newPlacementFixture(t)
	mount := repoMount(t, fx.m, "alpha")
	sid := initAt(t, fx.h, mount)

	for _, tc := range []struct{ topic, category, want string }{
		{"chain", "links", `validate path: unknown topic "chain"`},
		{"architecture", " ", "category is required"},
	} {
		t.Run(tc.topic, func(t *testing.T) {
			tip, paths := fx.tip(t), fx.paths(t)

			text, isErr := callToolAt(t, fx.h, mount, sid, "knomit_learn", fmt.Sprintf(
				`{"moment_name":"placement","facts":[{"topic":%q,"category":%q,"title":"A fact","body":"b"}]}`,
				tc.topic, tc.category))
			require.True(t, isErr, "MCP accepted it: %s", text)
			require.Contains(t, text, tc.want)

			domain, err := json.Marshal([]string{tc.topic, tc.category})
			require.NoError(t, err)
			fx.refused(t, fx.post(t, string(domain)), tc.want, tip, paths)
		})
	}

	// The positive control: both accept a declared topic.
	text, isErr := callToolAt(t, fx.h, mount, sid, "knomit_learn",
		`{"moment_name":"placement","facts":[{"topic":"architecture","category":"chain","title":"Via MCP","body":"b"}]}`)
	require.False(t, isErr, text)
	rec := fx.post(t, `["architecture","chain"]`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}
