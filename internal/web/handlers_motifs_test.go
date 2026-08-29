package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
		AuthoredClusters  int     `json:"authored_clusters"`
		AuthoredRecurring int     `json:"authored_recurring"`
		AuthoredMints     int     `json:"authored_mints"`
		AuthoredLinks     int     `json:"authored_links"`
		RecurrenceRate    float64 `json:"recurrence_rate"`

		AuthoredEpistemicRecurring int `json:"authored_epistemic_recurring"`
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
	if body.Health.AuthoredClusters != 3 || body.Health.AuthoredRecurring != 3 {
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

// TWO requests against ONE stub, deliberately: the name sort must order a COPY,
// never the slice the provider handed over. Sorting in place would leave the
// stub's own cluster list permanently reordered, so the df-desc request that
// follows would answer in name order — which is exactly what a reader of a
// caching provider would hit in production. Deleting the copy in
// handlers_motifs.go fails the second half of this test.
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

	// The default (df-desc) order must survive the name-sorted request above.
	body = decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs"))
	wantKeys := []string{"drift-config", "fallback-silent", "creep-scope"}
	for i, e := range body.Embedded.Motifs {
		if e.ClusterKey != wantKeys[i] {
			t.Fatalf("entry %d after a ?sort=name request: got %q, want %q — "+
				"the name sort reordered the provider's own slice",
				i, e.ClusterKey, wantKeys[i])
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

	// The ceiling is HARD, not a clamp (the params.go house rule): a client
	// that asked for 500 and silently got 200 rows cannot tell that from "there
	// were only 200".
	for _, bad := range []string{"limit=201", "limit=500", "limit=0", "limit=x", "offset=-1", "offset=x"} {
		if rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, rec.Code)
		}
	}
	// A refusal that says only "invalid" makes the caller guess the bound; the
	// ceiling refusal names the number, as limitParam's does.
	rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?limit=201")
	if !containsSub(rec.Body.String(), "limit must not exceed 200") {
		t.Errorf("over-ceiling detail must name the ceiling: %s", rec.Body.String())
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

// ── cluster detail ────────────────────────────────────────────────────────────

// motifDetailBody mirrors the cluster detail wire shape.
type motifDetailBody struct {
	ClusterKey      string   `json:"cluster_key"`
	Canonical       string   `json:"canonical"`
	Members         []string `json:"members"`
	DF              int      `json:"df"`
	Definition      string   `json:"definition"`
	DefinitionState string   `json:"definition_state"`
	CarrierCount    int      `json:"carrier_count"`
	Carriers        []struct {
		Path        string `json:"path"`
		Title       string `json:"title"`
		Type        string `json:"type"`
		CommittedAt int64  `json:"committed_at"`
	} `json:"carriers"`
	Aliases []struct {
		Motif     string `json:"motif"`
		Method    string `json:"method"`
		Rationale string `json:"rationale"`
	} `json:"aliases"`
	Links struct {
		Self struct {
			Href string `json:"href"`
		} `json:"self"`
		Facts struct {
			Href string `json:"href"`
		} `json:"facts"`
	} `json:"_links"`
}

func motifDetailServer(t *testing.T, stub *stubMotifsProvider, facts *stubFactsCollectionProvider) http.Handler {
	t.Helper()
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			motifs:          stub,
			factsCollection: facts,
		},
	}
	return s.NewAPIRouter()
}

func decodeMotifDetail(t *testing.T, rec *httptest.ResponseRecorder) motifDetailBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body motifDetailBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body
}

func driftClusterStub() *stubMotifsProvider {
	return &stubMotifsProvider{
		clusters: []store.MotifCluster{{
			ClusterKey:  "drift-config",
			CanonicalID: "config-drift",
			Members:     []string{"config-drift", "configuration-drifts"},
			DF:          4,
		}},
		defs: map[string]store.MotifDefinitionStatus{
			"drift-config": {Definition: "Configured state diverges from applied state."},
		},
		aliases: map[string]store.AliasRow{
			"config-drift": {
				CanonicalID: "config-drift", ClusterKey: "drift-config",
				Method: "mechanical",
			},
			"configuration-drifts": {
				CanonicalID: "config-drift", ClusterKey: "drift-config",
				Method: "judge", Rationale: "same mechanism",
			},
		},
	}
}

func driftCarriers() *stubFactsCollectionProvider {
	return &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{Path: "kb/a.md", Title: "Newest carrier", Type: "observation", CommittedAt: 200},
			{Path: "kb/b.md", Title: "Older carrier", Type: "policy", CommittedAt: 100},
		},
		// The corpus holds more carriers than this preview page shows.
		total: 4,
	}
}

func TestHandleHALMotifCluster_ByClusterKey(t *testing.T) {
	stub, facts := driftClusterStub(), driftCarriers()
	r := motifDetailServer(t, stub, facts)
	body := decodeMotifDetail(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs/drift-config"))

	if body.ClusterKey != "drift-config" || body.Canonical != "config-drift" {
		t.Errorf("identity: got %q/%q", body.ClusterKey, body.Canonical)
	}
	if len(body.Members) != 2 || body.DF != 4 {
		t.Errorf("members/df: got %v / %d", body.Members, body.DF)
	}
	if body.DefinitionState != "current" {
		t.Errorf("definition_state: got %q, want current", body.DefinitionState)
	}
	if len(body.Carriers) != 2 {
		t.Fatalf("carriers: got %d, want 2", len(body.Carriers))
	}
	if body.Carriers[0].Path != "kb/a.md" || body.Carriers[0].Title != "Newest carrier" {
		t.Errorf("carrier 0: got %+v", body.Carriers[0])
	}
	// carrier_count is the CORPUS total, not the page length — a preview that
	// reported its own length would claim the cluster has two carriers.
	if body.CarrierCount != 4 {
		t.Errorf("carrier_count: got %d, want 4 (the store's total)", body.CarrierCount)
	}
	if len(body.Aliases) != 2 {
		t.Fatalf("aliases: got %d, want 2", len(body.Aliases))
	}
	byMotif := map[string]string{}
	for _, a := range body.Aliases {
		byMotif[a.Motif] = a.Method + "/" + a.Rationale
	}
	if byMotif["config-drift"] != "mechanical/" {
		t.Errorf("mechanical row: got %q", byMotif["config-drift"])
	}
	if byMotif["configuration-drifts"] != "judge/same mechanism" {
		t.Errorf("judge row: got %q", byMotif["configuration-drifts"])
	}
	if !containsSub(body.Links.Self.Href, "/motifs/drift-config") {
		t.Errorf("self: got %q", body.Links.Self.Href)
	}
	wantFacts := "/facts?motif_match=exact&motifs=config-drift%2Cconfiguration-drifts"
	if !containsSub(body.Links.Facts.Href, wantFacts) {
		t.Errorf("facts link: got %q, want suffix %q", body.Links.Facts.Href, wantFacts)
	}

	// The carriers preview IS the pivot query: same filter, same tier, so the
	// preview can never disagree with the listing it links to.
	if got := strings.Join(facts.lastOpts.Motifs, ","); got != "config-drift,configuration-drifts" {
		t.Errorf("carriers query Motifs: got %v, want every member spelling", facts.lastOpts.Motifs)
	}
	if facts.lastOpts.MotifMatch != store.MotifMatchExact {
		t.Errorf("carriers query MotifMatch: got %q, want exact", facts.lastOpts.MotifMatch)
	}
	if facts.lastOpts.Limit != 20 {
		t.Errorf("carriers query Limit: got %d, want the default 20", facts.lastOpts.Limit)
	}
}

func TestHandleHALMotifCluster_BySpelling(t *testing.T) {
	stub := driftClusterStub()
	stub.clusterKeys = map[string]string{"configuration-drifts": "drift-config"}
	r := motifDetailServer(t, stub, driftCarriers())
	body := decodeMotifDetail(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs/configuration-drifts"))

	if body.ClusterKey != "drift-config" {
		t.Errorf("cluster_key: got %q, want drift-config", body.ClusterKey)
	}
	// The self link canonicalizes to the cluster key regardless of how the
	// reader arrived (C1).
	if !containsSub(body.Links.Self.Href, "/motifs/drift-config") {
		t.Errorf("self: got %q, want the cluster key", body.Links.Self.Href)
	}
}

func TestHandleHALMotifCluster_UnknownIs404(t *testing.T) {
	r := motifDetailServer(t, driftClusterStub(), driftCarriers())
	rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs/never-heard-of-it")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// An unrebuilt corpus has an empty alias table: every spelling is its own
// singleton cluster. That degradation is a 200 with one member and no audit
// rows — never a 404 (C2).
func TestHandleHALMotifCluster_SingletonOnUnrebuiltCorpus(t *testing.T) {
	stub := &stubMotifsProvider{
		clusters: []store.MotifCluster{{
			ClusterKey: "silent-fallback", CanonicalID: "silent-fallback",
			Members: []string{"silent-fallback"}, DF: 1,
		}},
		aliases: map[string]store.AliasRow{},
	}
	rec := getMotifs(t, motifDetailServer(t, stub, driftCarriers()),
		"/repos/alpha/branches/agent:test/motifs/silent-fallback")
	body := decodeMotifDetail(t, rec)

	if len(body.Members) != 1 || body.Members[0] != "silent-fallback" {
		t.Errorf("members: got %v", body.Members)
	}
	if body.DefinitionState != "missing" {
		t.Errorf("definition_state: got %q, want missing", body.DefinitionState)
	}
	if len(body.Aliases) != 1 || body.Aliases[0].Method != "" {
		t.Errorf("aliases: got %+v, want one member with no audit row", body.Aliases)
	}
	if !containsSub(rec.Body.String(), `"aliases":[`) {
		t.Errorf("aliases must serialize as an array, not null: %s", rec.Body.String())
	}
}

func TestHandleHALMotifCluster_CarriersLimitParam(t *testing.T) {
	stub := driftClusterStub()
	facts := driftCarriers()
	r := motifDetailServer(t, stub, facts)

	decodeMotifDetail(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs/drift-config?limit=5"))
	if facts.lastOpts.Limit != 5 {
		t.Errorf("limit=5: got %d", facts.lastOpts.Limit)
	}

	// The carrier ceiling is hard too: over it is a 400, never a quiet 100.
	for _, bad := range []string{"limit=101", "limit=500", "limit=0", "limit=x"} {
		if rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs/drift-config?"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, rec.Code)
		}
	}
	rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs/drift-config?limit=101")
	if !containsSub(rec.Body.String(), "limit must not exceed 100") {
		t.Errorf("over-ceiling detail must name the ceiling: %s", rec.Body.String())
	}
}

// An offset near MaxInt passes the `n < 0` parse guard, and then offset+limit
// WRAPS NEGATIVE — clearing every `< total` comparison written as a sum and
// reaching the slice expression as a negative bound. The result is a bounds
// panic on an unauthenticated GET, so the arithmetic must never form the sum.
// Mirrors TestLensFanoutDepth_OverflowingOffsetStillClamps.
func TestHandleHALMotifs_OverflowingOffsetIsAnEmptyPage(t *testing.T) {
	stub := &stubMotifsProvider{clusters: threeClusters()}
	r := motifsServer(t, stub)

	// Three DISTINCT points, each genuinely inside the overflow window FOR ITS
	// OWN LIMIT — the window is offset > MaxInt-limit, so it moves with the
	// limit and a case must carry the limit that puts it inside. The third
	// lands the old sum exactly on MaxInt+1, the shallowest overflow there is.
	for _, tc := range []struct{ name, query string }{
		{"MaxInt at the default limit", "offset=" + strconv.Itoa(math.MaxInt)},
		{"MaxInt-10 at the default limit", "offset=" + strconv.Itoa(math.MaxInt-10)},
		{"overflows exactly at MaxInt+1", "offset=" + strconv.Itoa(math.MaxInt-199) + "&limit=200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?"+tc.query)
			body := decodeMotifs(t, rec)
			if len(body.Embedded.Motifs) != 0 {
				t.Errorf("page: got %d entries, want none past the end", len(body.Embedded.Motifs))
			}
			if body.Count != 3 {
				t.Errorf("count: got %d, want the unnarrowed total 3", body.Count)
			}
			if body.Links.Next != nil {
				t.Errorf("next must be absent past the end: %q", body.Links.Next.Href)
			}
		})
	}
}

// `count` and the health block count DIFFERENT POPULATIONS, and the wire names
// have to say so. count is every cluster from the vocabulary query and is
// narrowed by ?q=; health is computed over AUTHORED facts only and is NOT
// narrowed. Two numbers that disagree for two good reasons are a bug report
// waiting to happen unless the names carry the qualifier.
func TestHandleHALMotifs_HealthNamesItsPopulationAndIgnoresQ(t *testing.T) {
	stub := &stubMotifsProvider{
		clusters: threeClusters(),
		// Fewer than len(clusters): the authored-only population is smaller,
		// exactly as a corpus with distilled facts reports.
		health: store.MotifVocabularyHealth{Clusters: 2, Recurring: 1, Mints: 2, Links: 3, EpistemicRecurring: 1},
	}
	r := motifsServer(t, stub)

	body := decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs"))
	if body.Count != 3 {
		t.Errorf("count: got %d, want 3 (every cluster)", body.Count)
	}
	if body.Health.AuthoredClusters != 2 {
		t.Errorf("authored_clusters: got %d, want 2 (the authored-only population)", body.Health.AuthoredClusters)
	}
	if body.Health.AuthoredRecurring != 1 || body.Health.AuthoredMints != 2 || body.Health.AuthoredLinks != 3 {
		t.Errorf("health: got %+v", body.Health)
	}

	// ?q= narrows count. It does NOT narrow health — the health block is a
	// corpus-level diagnostic, not a description of the page.
	narrowed := decodeMotifs(t, getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs?q=fallback"))
	if narrowed.Count != 1 {
		t.Errorf("narrowed count: got %d, want 1", narrowed.Count)
	}
	if narrowed.Health.AuthoredClusters != 2 {
		t.Errorf("authored_clusters under ?q=: got %d, want the unnarrowed 2", narrowed.Health.AuthoredClusters)
	}

	// The epistemic count is authored AND epistemic — VocabularyHealth
	// computes it with a kind CASE inside the same origin='authored' filter —
	// so it carries the prefix too.
	if body.Health.AuthoredEpistemicRecurring != 1 {
		t.Errorf("authored_epistemic_recurring: got %d, want 1", body.Health.AuthoredEpistemicRecurring)
	}

	// The unqualified names must be gone from the wire: a reader who sees
	// "clusters" beside "count" reads them as the same population.
	raw := getMotifs(t, r, "/repos/alpha/branches/agent:test/motifs").Body.String()
	for _, gone := range []string{`"clusters"`, `"recurring"`, `"mints"`, `"links"`, `"epistemic_recurring"`} {
		if containsSub(raw, gone) {
			t.Errorf("unqualified health field %s is still on the wire: %s", gone, raw)
		}
	}
}
