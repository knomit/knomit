package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"

	"github.com/stretchr/testify/require"
)

// stubFactsCollectionProvider implements factsCollectionProvider for tests.
type stubFactsCollectionProvider struct {
	entries  []store.RecentFactEntry
	total    int
	err      error
	lastOpts store.SearchOptions
}

func (s *stubFactsCollectionProvider) RecentFacts(
	_ context.Context,
	_ *repos.RepoInstance, _ string, opts store.SearchOptions,
) ([]store.RecentFactEntry, int, error) {
	s.lastOpts = opts
	return s.entries, s.total, s.err
}

func TestHandleHALFactsCollection_ReturnsHALCollection(t *testing.T) {
	provider := &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{Path: "know/a.md", Title: "Fact A", Type: "observation", CommittedAt: 1700000000, Operation: "learn"},
			{Path: "know/b.md", Title: "Fact B", Type: "hypothesis", CommittedAt: 1700000001, Operation: "update"},
		},
		total: 2,
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Facts []struct {
				Path        string      `json:"path"`
				Title       string      `json:"title"`
				Type        string      `json:"type"`
				CommittedAt int64       `json:"committed_at"`
				Operation   string      `json:"operation"`
				Links       hal.LinkMap `json:"_links"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if len(body.Embedded.Facts) != 2 {
		t.Fatalf("facts: %d, want 2", len(body.Embedded.Facts))
	}

	f := body.Embedded.Facts[0]
	if f.Path != "know/a.md" {
		t.Errorf("path: %q", f.Path)
	}
	if f.Operation != "learn" {
		t.Errorf("operation: %q", f.Operation)
	}
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/facts/know/a.md"
	if got := f.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALFactsCollection_Pagination(t *testing.T) {
	// 3 items total, return 1, offset=1 → both next and prev links expected.
	provider := &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{Path: "know/b.md", Title: "Fact B"},
		},
		total: 3,
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?limit=1&offset=1", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}

	var body struct {
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body.Links["next"]; !ok {
		t.Error("missing next link")
	}
	if _, ok := body.Links["prev"]; !ok {
		t.Error("missing prev link")
	}
}

// TestHandleHALFactsCollection_SerializesDomainAndEntities verifies that the
// HAL endpoint surfaces a fact's domain and entities slices on the wire when
// the store returns them. The bridge's SessionStart hook filters by these
// fields client-side; without serialization the bridge silently sees empty
// arrays and never renders the PROJECT PRINCIPLES block.
func TestHandleHALFactsCollection_SerializesDomainAndEntities(t *testing.T) {
	provider := &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{
				Path:     "kb/policy/p.md",
				Title:    "Principle",
				Type:     "policy",
				Domain:   []string{"global"},
				Entities: []string{"designer"},
			},
		},
		total: 1,
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var body struct {
		Embedded struct {
			Facts []struct {
				Path     string   `json:"path"`
				Domain   []string `json:"domain"`
				Entities []string `json:"entities"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Embedded.Facts, 1)
	require.Equal(t, []string{"global"}, body.Embedded.Facts[0].Domain,
		"domain slice must appear in HAL response so the bridge can filter on it")
	require.Equal(t, []string{"designer"}, body.Embedded.Facts[0].Entities,
		"entities slice must appear in HAL response so the bridge can filter on it")
}

// TestHandleHALFactsCollection_KindSerialization covers the read-side
// surfacing of fact.Kind: pragmatic entries carry "kind":"pragmatic" on
// the wire while epistemic entries elide the field (mirrors
// fact.Fact.MarshalJSON).
func TestHandleHALFactsCollection_KindSerialization(t *testing.T) {
	provider := &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{Path: "know/a.md", Title: "Fact A", Kind: "epistemic", Type: "observation"},
			{Path: "know/b.md", Title: "Fact B", Kind: "pragmatic", Type: "policy"},
		},
		total: 2,
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Parse the raw JSON to inspect the on-wire shape: epistemic should
	// have no "kind" key at all while pragmatic should have "kind":"pragmatic".
	var body struct {
		Embedded struct {
			Facts []map[string]interface{} `json:"facts"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Embedded.Facts, 2)

	// First fact is epistemic — "kind" key must be absent.
	_, hasKind := body.Embedded.Facts[0]["kind"]
	require.False(t, hasKind, "epistemic fact must not carry kind field on the wire: %v", body.Embedded.Facts[0])

	// Second fact is pragmatic — "kind" must be present and equal "pragmatic".
	require.Equal(t, "pragmatic", body.Embedded.Facts[1]["kind"], "pragmatic fact must carry kind=pragmatic")
}

// TestHandleHALFactsCollection_KindFilterReachesProvider verifies that the
// ?kind= / ?exclude_kind= query params are parsed into SearchOptions.IncludeKinds
// / ExcludeKinds and reach the provider unchanged.
func TestHandleHALFactsCollection_KindFilterReachesProvider(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?kind=pragmatic&exclude_kind=epistemic", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"pragmatic"}, provider.lastOpts.IncludeKinds)
	require.Equal(t, []string{"epistemic"}, provider.lastOpts.ExcludeKinds)
}

// ?origin= query param is parsed into SearchOptions.IncludeOrigins and reaches
// the provider unchanged (CSV values split).
func TestHandleHALFactsCollection_OriginFilterReachesProvider(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?origin=distilled,discovered", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"distilled", "discovered"}, provider.lastOpts.IncludeOrigins)
}

// TestHandleHALFactsCollection_TopicReachesProvider verifies ?topic=X is
// translated to the conventional path prefix kb/X/.
func TestHandleHALFactsCollection_TopicReachesProvider(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?topic=invariants", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "kb/invariants/", provider.lastOpts.Path)
}

// TestHandleHALFactsCollection_ExplicitPathBeatsTopic verifies that an
// explicit ?path= wins over ?topic= so callers can override the convention.
func TestHandleHALFactsCollection_ExplicitPathBeatsTopic(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?topic=invariants&path=custom/", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "custom/", provider.lastOpts.Path)
}

// TestHandleHALFactsCollection_EntitySingularReachesProvider verifies that
// ?entity=X (singular — the canonical name advertised in the HAL template)
// reaches the provider's Entities field.
func TestHandleHALFactsCollection_EntitySingularReachesProvider(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?entity=Service.Verify", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, []string{"Service.Verify"}, provider.lastOpts.Entities)
}

// TestHandleHALFactsCollection_EntitySingularAndPluralMerge verifies that
// ?entity=X&entities=Y,Z merges to all three values (singular is the canonical
// form, plural is accepted as alias).
func TestHandleHALFactsCollection_EntitySingularAndPluralMerge(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?entity=A&entities=B,C", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.ElementsMatch(t, []string{"A", "B", "C"}, provider.lastOpts.Entities)
}

// TestHandleHALFactsCollection_MinConfidenceReachesProvider verifies that
// ?min_confidence=0.8 is parsed and forwarded.
func TestHandleHALFactsCollection_MinConfidenceReachesProvider(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?min_confidence=0.8", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.InDelta(t, 0.8, provider.lastOpts.MinConfidence, 1e-9)
}

// TestHandleHALFactsCollection_InvalidMinConfidence_Returns400 verifies that
// a non-numeric min_confidence yields a problem+json 400.
func TestHandleHALFactsCollection_InvalidMinConfidence_Returns400(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: &stubFactsCollectionProvider{},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?min_confidence=notanumber", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleHALFactsCollection_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t),
		providers: storeProviders{
			factsCollection: &stubFactsCollectionProvider{},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

func TestHandleHALFactsCollection_StoreError_Returns500(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: &stubFactsCollectionProvider{err: errors.New("db error")},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500", rec.Code)
	}
}

// TestHandleHALFactsCollection_MotifFilterReachesSearchOptions locks in that
// ?motifs= / ?motif_match= on /facts reach SearchOptions.
func TestHandleHALFactsCollection_MotifFilterReachesSearchOptions(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?motifs=zero-value-as-valid,silent-fallback&motif_match=stem", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"zero-value-as-valid", "silent-fallback"}, provider.lastOpts.Motifs)
	require.Equal(t, store.MotifMatchStem, provider.lastOpts.MotifMatch)
}

// TestHandleHALFactsCollection_LooseMotifTierRejected: refused at the edge,
// before any store call (C3/MN6).
func TestHandleHALFactsCollection_LooseMotifTierRejected(t *testing.T) {
	provider := &stubFactsCollectionProvider{}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?motifs=a-b&motif_match=token-1", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Nil(t, provider.lastOpts.Motifs, "provider must not be called on a rejected tier")
}

// TestHandleHALFactsCollection_MotifsOnTheWire: rows carry their motifs, and a
// motif-free row omits the key entirely.
func TestHandleHALFactsCollection_MotifsOnTheWire(t *testing.T) {
	provider := &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{Path: "kb/a.md", Title: "Carries a motif", Motifs: []string{"a-b"}},
			{Path: "kb/b.md", Title: "Carries none"},
		},
		total: 2,
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factsCollection: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test/facts", nil)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Embedded struct {
			Facts []map[string]any `json:"facts"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Len(t, view.Embedded.Facts, 2)
	require.Equal(t, []any{"a-b"}, view.Embedded.Facts[0]["motifs"])
	_, present := view.Embedded.Facts[1]["motifs"]
	require.False(t, present, "motif-free row must omit the key entirely")
}
