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

	"knomit/internal/config"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
	"knomit/internal/testsupport/playbooks"
)

// templatesServer is an API router over a Manager that mounts the pinned
// knomit-playbooks checkout as "playbooks" (a clone of a local bare repo).
func templatesServer(t *testing.T, mountPlaybooks bool) (http.Handler, *repos.Manager) {
	t.Helper()
	return templatesServerAt(t, t.TempDir(), mountPlaybooks)
}

// templatesServerAt is templatesServer with filesystem origins allowed under
// root.
func templatesServerAt(t *testing.T, root string, mountPlaybooks bool) (http.Handler, *repos.Manager) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb", LocalOriginRoot: root},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	if mountPlaybooks {
		bare := playbooks.SourceRepo(t, filepath.Join(root, "playbooks.git"), "main", nil)
		_, err := m.Create(context.Background(), repos.CreateSpec{Name: "playbooks", Mode: "clone",
			Origin: &repos.OriginSpec{URL: fileuri.New(bare)}}, nil)
		require.NoError(t, err)
	}
	s := &Server{Manager: m}
	return s.NewAPIRouter(), m
}

func getTemplates(t *testing.T, r http.Handler, path string) (int, []repos.TemplateInfo) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodGet, path, nil)))
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var body struct {
		Templates []repos.TemplateInfo `json:"templates"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotNil(t, body.Templates, "an empty listing is [], never null")
	return rec.Code, body.Templates
}

// F24 Verification "No automatic mount": a fresh home lists no templates
// (an empty array) and still offers the embedded presets.
func TestTemplates_EmptyOnFreshHome(t *testing.T) {
	r, _ := templatesServer(t, false)
	code, got := getTemplates(t, r, "/templates")
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, got)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodGet, "/ontologies/presets", nil)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"default"`)
}

// F24 listing routes: all repos, one repo, and the per-repo statuses (a lens
// name is 404 through RepoMiddleware, an unknown repo 404).
func TestTemplates_ListingRoutes(t *testing.T) {
	r, m := templatesServer(t, true)
	code, all := getTemplates(t, r, "/templates")
	require.Equal(t, http.StatusOK, code)
	var names []string
	for _, ti := range all {
		require.Equal(t, "playbooks", ti.Repo)
		require.Len(t, ti.Commit, 40)
		require.NotEmpty(t, ti.Description)
		require.True(t, strings.HasPrefix(ti.Fact, "kb/templates/"+ti.Name+"/"), ti.Fact)
		names = append(names, ti.Name)
	}
	require.Equal(t, []string{"coding", "fleet", "general", "mission"}, names)

	code, one := getTemplates(t, r, "/repos/playbooks/templates")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, all, one)

	code, _ = getTemplates(t, r, "/repos/nope/templates")
	require.Equal(t, http.StatusNotFound, code)

	_, err := m.CreateLens(context.Background(), repos.Lens{Name: "a-lens", WriteUID: m.Get("playbooks").UID()})
	require.NoError(t, err)
	code, _ = getTemplates(t, r, "/repos/a-lens/templates")
	require.Equal(t, http.StatusNotFound, code)
}

// F24 create over REST: the template body field, the refusals as statuses
// before any job starts, and a create that succeeds.
// SABOTAGE: drop the `template` field from the request decode → the 202 row
// fails with 400 ("template mode requires template").
func TestPostRepos_Template(t *testing.T) {
	r, m := templatesServer(t, true)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, "/repos", strings.NewReader(body))))
		return rec
	}
	for _, tc := range []struct {
		body   string
		status int
		detail string
	}{
		{`{"name":"a","mode":"template","template":{"name":"mission"}}`, http.StatusBadRequest, "template.repo"},
		{`{"name":"a","mode":"template","template":{"repo":"playbooks","name":"../x"}}`, http.StatusBadRequest, "template name"},
		{`{"name":"a","mode":"template","template":{"repo":"nope","name":"mission"}}`, http.StatusNotFound, "not mounted"},
		{`{"name":"a","mode":"template","template":{"repo":"playbooks","name":"absent"}}`, http.StatusNotFound, "template not found"},
		{`{"name":"a","mode":"template","template":{"repo":"playbooks","name":"fleet"}}`, http.StatusConflict, "initialize"},
		{`{"name":"a","mode":"clone","template":{"repo":"playbooks","name":"mission"},"origin":{"url":"https://example.com/x.git"}}`, http.StatusBadRequest, "template"},
		{`{"name":"a","mode":"preset","ontology_preset":"default","template":{"repo":"playbooks","name":"mission"}}`, http.StatusBadRequest, "template"},
	} {
		rec := post(tc.body)
		require.Equal(t, tc.status, rec.Code, "%s → %s", tc.body, rec.Body.String())
		require.Contains(t, rec.Body.String(), tc.detail, tc.body)
		require.Nil(t, m.Get("a"))
	}

	rec := post(`{"name":"my-mission","mode":"template","template":{"repo":"playbooks","name":"mission"}}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	final := awaitCreate(t, r, rec)
	require.Equal(t, "done", final["state"], final)
	require.Equal(t, "mission", m.Get("my-mission").Ontology().ID)
}

// A 422 for a template that is there and unusable: covered per rule in
// internal/repos; here only the status mapping of each sentinel.
func TestCreateErrStatus_TemplateSentinels(t *testing.T) {
	for _, err := range []error{repos.ErrTemplateNotRegular, repos.ErrTemplateTooLarge, repos.ErrTemplateLayout,
		repos.ErrTemplateDescribedTwice, repos.ErrTemplateNoOntology, repos.ErrTemplateOntology, repos.ErrTemplatePresetID} {
		status, title := createErrStatus(err)
		require.Equal(t, http.StatusUnprocessableEntity, status, err.Error())
		require.Equal(t, "Template invalid", title)
	}
	status, _ := createErrStatus(repos.ErrTemplateSourceUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, status)
	status, title := createErrStatus(repos.ErrTemplateFleetStateUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "Fleet state unavailable", title)
}

// N5 (#433 review): the two fleet-template 409s carry different titles, and
// the title comes from the ERROR the create returned: a fleet template as a
// LOCAL repo is told to use initialize; an initialize on an instance that
// already has a fleet is NOT (it already is one). Both run the real
// checkTemplateMode through POST /repos, not a hand-built sentinel.
// SABOTAGE: put the ErrTemplateFleetLocal arm before ErrTemplateFleetPresent
// in createErrStatus → the second-fleet row reads "needs initialize" → red.
func TestPostRepos_FleetTemplateTitles(t *testing.T) {
	root := t.TempDir()
	r, m := templatesServerAt(t, root, true)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, "/repos", strings.NewReader(body))))
		return rec
	}
	title := func(rec *httptest.ResponseRecorder) string {
		var p struct {
			Title string `json:"title"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
		return p.Title
	}
	initFleet := func(name, bare string) string {
		url := seedBareRemoteForTest(t, filepath.Join(root, bare))
		return `{"name":"` + name + `","mode":"initialize","origin":{"url":"` + url + `"},"template":{"repo":"playbooks","name":"fleet"}}`
	}

	rec := post(`{"name":"f0","mode":"template","template":{"repo":"playbooks","name":"fleet"}}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "Fleet template needs initialize", title(rec))

	rec = post(initFleet("fleet", "fleet1.git"))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, "done", awaitCreate(t, r, rec)["state"])
	require.True(t, m.IsFleetRepo("fleet"), "reached: the instance now has a fleet")

	rec = post(initFleet("fleet2", "fleet2.git"))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "Fleet already present", title(rec))
	require.Contains(t, rec.Body.String(), `\"fleet\"`, "the detail names the fleet repository")
	require.Nil(t, m.Get("fleet2"))
}
