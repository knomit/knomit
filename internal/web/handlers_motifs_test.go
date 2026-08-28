package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// stubMotifsProvider implements motifsProvider for tests.
type stubMotifsProvider struct {
	clusters    []store.MotifCluster
	clustersErr error
	defs        map[string]store.MotifDefinitionStatus
	health      store.MotifVocabularyHealth
	aliases     map[string]store.AliasRow
	clusterKeys map[string]string // spelling → cluster key for ClusterKey()
}

func (s *stubMotifsProvider) Clusters(_ context.Context, _ *repos.RepoInstance, _ string) ([]store.MotifCluster, error) {
	return s.clusters, s.clustersErr
}

func (s *stubMotifsProvider) Definitions(_ context.Context, _ *repos.RepoInstance, _ string, _ []string) (map[string]store.MotifDefinitionStatus, error) {
	return s.defs, nil
}

func (s *stubMotifsProvider) VocabularyHealth(_ context.Context, _ *repos.RepoInstance, _ string) (store.MotifVocabularyHealth, error) {
	return s.health, nil
}

func (s *stubMotifsProvider) AliasRows(_ context.Context, _ *repos.RepoInstance, _ string) (map[string]store.AliasRow, error) {
	return s.aliases, nil
}

func (s *stubMotifsProvider) ClusterKey(_ context.Context, _ *repos.RepoInstance, _, motif string) (string, error) {
	if k, ok := s.clusterKeys[motif]; ok {
		return k, nil
	}
	return motif, nil // mirror the store: unresolved spelling degrades to itself
}

// motifsCollectionBody mirrors the vocabulary collection wire shape.
type motifsCollectionBody struct {
	Count  int `json:"count"`
	Health struct {
		Clusters       int     `json:"clusters"`
		Recurring      int     `json:"recurring"`
		RecurrenceRate float64 `json:"recurrence_rate"`
	} `json:"health"`
	Links struct {
		Self struct {
			Href string `json:"href"`
		} `json:"self"`
		Next *struct {
			Href string `json:"href"`
		} `json:"next"`
		Prev *struct {
			Href string `json:"href"`
		} `json:"prev"`
	} `json:"_links"`
	Embedded struct {
		Motifs []struct {
			ClusterKey      string   `json:"cluster_key"`
			Canonical       string   `json:"canonical"`
			Members         []string `json:"members"`
			DF              int      `json:"df"`
			Definition      string   `json:"definition"`
			DefinitionState string   `json:"definition_state"`
			Links           struct {
				Self struct {
					Href string `json:"href"`
				} `json:"self"`
			} `json:"_links"`
		} `json:"motifs"`
	} `json:"_embedded"`
}

func motifsServer(t *testing.T, stub *stubMotifsProvider) http.Handler {
	t.Helper()
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			motifs: stub,
		},
	}
	return s.NewAPIRouter()
}

func getMotifs(t *testing.T, r http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func decodeMotifs(t *testing.T, rec *httptest.ResponseRecorder) motifsCollectionBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body motifsCollectionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body
}

// threeClusters is the df-desc order Clusters already returns.
func threeClusters() []store.MotifCluster {
	return []store.MotifCluster{
		{ClusterKey: "drift-config", CanonicalID: "config-drift", Members: []string{"config-drift", "configuration-drifts"}, DF: 5},
		{ClusterKey: "fallback-silent", CanonicalID: "silent-fallback", Members: []string{"silent-fallback"}, DF: 2},
		{ClusterKey: "creep-scope", CanonicalID: "scope-creep", Members: []string{"scope-creep"}, DF: 2},
	}
}

func TestHandleHALMotifs_ReturnsRankedCollection(t *testing.T) {
	stub := &stubMotifsProvider{
		clusters: threeClusters(),
		defs: map[string]store.MotifDefinitionStatus{
			"drift-config":    {Definition: "Configured state diverges from applied state."},
			"fallback-silent": {Definition: "A component keeps serving after a dependency fails.", Stale: true},
			// creep-scope is deliberately absent: never defined.
		},
		health: store.MotifVocabularyHealth{Clusters: 3, Recurring: 3, Mints: 3, Links: 6, EpistemicRecurring: 2},
	}
	body := decodeMotifs(t, getMotifs(t, motifsServer(t, stub), "/repos/alpha/branches/agent:test/motifs"))

	if body.Count != 3 {
		t.Errorf("count: got %d, want 3", body.Count)
	}
	if body.Health.Clusters != 3 || body.Health.Recurring != 3 {
		t.Errorf("health: got %+v", body.Health)
	}
	if body.Health.RecurrenceRate != 1 {
		t.Errorf("recurrence_rate: got %v, want 1", body.Health.RecurrenceRate)
	}
	entries := body.Embedded.Motifs
	if len(entries) != 3 {
		t.Fatalf("entries: got %d, want 3", len(entries))
	}
	wantKeys := []string{"drift-config", "fallback-silent", "creep-scope"}
	wantStates := []string{"current", "stale", "missing"}
	for i, e := range entries {
		if e.ClusterKey != wantKeys[i] {
			t.Errorf("entry %d cluster_key: got %q, want %q", i, e.ClusterKey, wantKeys[i])
		}
		if e.DefinitionState != wantStates[i] {
			t.Errorf("entry %d definition_state: got %q, want %q", i, e.DefinitionState, wantStates[i])
		}
		want := "/repos/alpha/branches/agent:test/motifs/" + wantKeys[i]
		if got := e.Links.Self.Href; len(got) < len(want) || got[len(got)-len(want):] != want {
			t.Errorf("entry %d self: got %q, want suffix %q", i, got, want)
		}
	}
	// Humans read the canonical; the URL keys on the cluster key (C1).
	if entries[0].Canonical != "config-drift" {
		t.Errorf("canonical: got %q, want config-drift", entries[0].Canonical)
	}
	if entries[0].DF != 5 {
		t.Errorf("df: got %d, want 5", entries[0].DF)
	}
	if len(entries[0].Members) != 2 {
		t.Errorf("members: got %v, want both spellings", entries[0].Members)
	}
	if entries[2].Definition != "" {
		t.Errorf("missing definition must be empty, got %q", entries[2].Definition)
	}
}

func TestHandleHALMotifs_SortName(t *testing.T) {
	stub := &stubMotifsProvider{clusters: threeClusters()}
	r := motifsServer(t, stub)

	body := decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?sort=name"))
	want := []string{"config-drift", "scope-creep", "silent-fallback"}
	for i, e := range body.Embedded.Motifs {
		if e.Canonical != want[i] {
			t.Errorf("entry %d canonical: got %q, want %q", i, e.Canonical, want[i])
		}
	}

	if rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?sort=bogus"); rec.Code != http.StatusBadRequest {
		t.Errorf("sort=bogus: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALMotifs_NarrowByQ(t *testing.T) {
	stub := &stubMotifsProvider{
		clusters: threeClusters(),
		defs: map[string]store.MotifDefinitionStatus{
			// The definition, not any member spelling, is what "diverges" hits.
			"drift-config": {Definition: "Configured state diverges from applied state."},
		},
	}
	r := motifsServer(t, stub)

	body := decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?q=FALLBACK"))
	if body.Count != 1 {
		t.Fatalf("count: got %d, want 1 (count reflects the narrowed total)", body.Count)
	}
	if body.Embedded.Motifs[0].ClusterKey != "fallback-silent" {
		t.Errorf("kept the wrong cluster: %q", body.Embedded.Motifs[0].ClusterKey)
	}

	body = decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?q=diverges"))
	if body.Count != 1 || body.Embedded.Motifs[0].ClusterKey != "drift-config" {
		t.Errorf("definition text must be searchable: count=%d entries=%+v", body.Count, body.Embedded.Motifs)
	}
}

func TestHandleHALMotifs_Paging(t *testing.T) {
	stub := &stubMotifsProvider{clusters: threeClusters()}
	r := motifsServer(t, stub)

	body := decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?limit=2"))
	if len(body.Embedded.Motifs) != 2 {
		t.Errorf("page: got %d entries, want 2", len(body.Embedded.Motifs))
	}
	if body.Count != 3 {
		t.Errorf("count: got %d, want 3 (the total, not the page)", body.Count)
	}
	if body.Links.Next == nil {
		t.Fatal("next link missing while a third cluster remains")
	}
	if !containsSub(body.Links.Next.Href, "offset=2") {
		t.Errorf("next href: got %q, want offset=2", body.Links.Next.Href)
	}
	if body.Links.Prev != nil {
		t.Errorf("prev must be absent on the first page: %q", body.Links.Prev.Href)
	}

	body = decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?limit=2&offset=2"))
	if len(body.Embedded.Motifs) != 1 {
		t.Errorf("last page: got %d entries, want 1", len(body.Embedded.Motifs))
	}
	if body.Links.Next != nil {
		t.Errorf("next must be absent on the last page: %q", body.Links.Next.Href)
	}
	if body.Links.Prev == nil {
		t.Error("prev link missing on the second page")
	}

	// Over the ceiling clamps rather than erroring.
	if rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?limit=500"); rec.Code != http.StatusOK {
		t.Errorf("limit=500: got %d, want 200 (clamped, not refused), body=%s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{"limit=0", "limit=x", "offset=-1", "offset=x"} {
		if rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, rec.Code)
		}
	}
}

func TestHandleHALMotifs_EmptyVocabulary(t *testing.T) {
	rec := getMotifs(t, motifsServer(t, &stubMotifsProvider{}), "/repos/alpha/branches/agent:test/motifs")
	body := decodeMotifs(t, rec)
	if body.Count != 0 {
		t.Errorf("count: got %d, want 0", body.Count)
	}
	if !containsSub(rec.Body.String(), `"motifs":[]`) {
		t.Errorf("empty vocabulary must serialize an empty array, not null: %s", rec.Body.String())
	}
}

// containsSub is strings.Contains, kept local so the test file's intent reads
// without an import alias collision.
func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
