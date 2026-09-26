package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

var webExpNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// pinClock fixes the web layer's clock for one test.
func pinClock(t *testing.T) {
	t.Helper()
	prev := timeNow
	timeNow = func() time.Time { return webExpNow }
	t.Cleanup(func() { timeNow = prev })
}

func seedDatedFact(t *testing.T, ri *repos.RepoInstance, path, expires string) {
	t.Helper()
	exp := ""
	if expires != "" {
		exp = "expires: \"" + expires + "\"\n"
	}
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path,
			"---\ntype: hypothesis\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: []\n"+exp+"---\n# "+path+"\n\nbody\n",
			"add "+path, "")
		require.NoError(t, err)
	}))
}

// datedRepo seeds past / soon / never under kb/technology/dated/.
func datedRepo(t *testing.T) (*repos.Manager, *repos.RepoInstance, http.Handler) {
	t.Helper()
	pinClock(t)
	m, _ := newTestLensManager(t, "alpha", "beta")
	ri := m.Get("alpha")
	seedDatedFact(t, ri, "kb/technology/dated/past.md", webExpNow.Add(-time.Hour).Format(time.RFC3339))
	seedDatedFact(t, ri, "kb/technology/dated/soon.md", webExpNow.Add(24*time.Hour).Format(time.RFC3339))
	seedDatedFact(t, ri, "kb/technology/dated/never.md", "")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	return m, ri, r
}

type datedRow struct {
	Path    string `json:"path"`
	Expires string `json:"expires"`
	Expired bool   `json:"expired"`
}

func getExpiryJSON(t *testing.T, r http.Handler, url string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

// rowsOf extracts rows from a HAL collection (_embedded.<key>) or a plain
// top-level list (lens search's "results", lens facts' "facts").
func rowsOf(t *testing.T, rec *httptest.ResponseRecorder, key string) map[string]datedRow {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var env struct {
		Embedded map[string][]datedRow `json:"_embedded"`
		Facts    []datedRow            `json:"facts"`
		Results  []datedRow            `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	rows := env.Embedded[key]
	if rows == nil {
		rows = append(env.Facts, env.Results...)
	}
	out := map[string]datedRow{}
	for _, row := range rows {
		// Lens rows are wire paths; the write repo's are bare.
		out[row.Path] = row
	}
	return out
}

func names(rows map[string]datedRow) []string {
	var out []string
	for p := range rows {
		p = strings.TrimSuffix(p[strings.LastIndex(p, "/")+1:], ".md")
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// TestREST_ExpiryFiltersAndMarkers: every surface that shares the filter
// builder accepts the three params with the documented semantics, hides
// nothing by default, and marks expired rows.
func TestREST_ExpiryFiltersAndMarkers(t *testing.T) {
	m, ri, r := datedRepo(t)
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	b := urlBranch(ri.AgentBranch())
	before := webExpNow.Add(48 * time.Hour).Format(time.RFC3339)
	surfaces := []struct {
		name, base, key string
	}{
		{"facts collection", "/repos/alpha/branches/" + b + "/facts?path=kb/technology/dated/", "facts"},
		{"search", "/repos/alpha/branches/" + b + "/search?path=kb/technology/dated/", "results"},
		{"lens facts", "/lenses/eng/facts?path=kb/technology/dated/", "facts"},
		{"lens search", "/lenses/eng/search?type=hypothesis", "results"},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			get := func(q string) map[string]datedRow {
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.base+q, nil))
				return rowsOf(t, rec, s.key)
			}
			all := get("")
			require.Equal(t, []string{"never", "past", "soon"}, names(all), "nothing hidden by default")
			for p, row := range all {
				require.Equal(t, strings.HasSuffix(p, "past.md"), row.Expired, p)
				require.Equal(t, strings.HasSuffix(p, "never.md"), row.Expires == "", p)
			}
			require.Equal(t, []string{"past"}, names(get("&expired=true")))
			require.Equal(t, []string{"never", "soon"}, names(get("&expired=false")), "expired=false INCLUDES the undated")
			require.Equal(t, []string{"past", "soon"}, names(get("&expires_before="+before)), "expires_before alone EXCLUDES the undated")
			require.Equal(t, []string{"soon"}, names(get("&expires_after="+webExpNow.Format(time.RFC3339)+"&expires_before="+before)), "window idiom")

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.base+"&expires_before=2026-10-01", nil))
			require.Equal(t, http.StatusBadRequest, rec.Code, "date-only bound is a 400")
			rec = httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.base+"&expired=maybe", nil))
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// TestREST_FactReadCarriesExpiry: the single-fact view carries the value and
// the marker; by-path reads are never filtered.
func TestREST_FactReadCarriesExpiry(t *testing.T) {
	_, ri, r := datedRepo(t)
	b := urlBranch(ri.AgentBranch())
	rec, body := getExpiryJSON(t, r, "/repos/alpha/branches/"+b+"/facts/kb/technology/dated/past.md")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, true, body["expired"])
	require.Equal(t, webExpNow.Add(-time.Hour).Format(time.RFC3339), body["expires"])

	_, body = getExpiryJSON(t, r, "/repos/alpha/branches/"+b+"/facts/kb/technology/dated/soon.md")
	require.Nil(t, body["expired"], "expired is omitted when false")
	require.NotEmpty(t, body["expires"])
}

// TestREST_WritesValidateExpires: POST accepts expires like knomit_learn and
// refuses a bad one; a PUT whose raw bytes carry a bad one is refused (the
// write side is strict even though parsing is lenient).
func TestREST_WritesValidateExpires(t *testing.T) {
	_, ri, r := datedRepo(t)
	b := urlBranch(ri.AgentBranch())
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+b+"/facts", strings.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}
	rec := post(`{"title":"Dated","body":"b","type":"hypothesis","domain":["technology","dated"],"expires":"2027-01-01T00:00:00Z"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var view map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Equal(t, "2027-01-01T00:00:00Z", view["expires"])

	rec = post(`{"title":"Bad","body":"b","type":"hypothesis","domain":["technology","dated"],"expires":"2027-01-01"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid expires")

	put := `{"content":"---\ntype: hypothesis\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: []\nexpires: 2027-01-01\n---\n# P\n\nbody\n"}`
	rec = httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/branches/"+b+"/facts/kb/technology/dated/never.md", strings.NewReader(put)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "2027-01-01")
}
