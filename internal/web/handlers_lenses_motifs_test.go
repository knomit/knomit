package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// lensMotifsStub is a per-repo motifsProvider: each mount's call is answered
// from a map keyed by the repo's name, so a single stub drives the whole
// fan-out. It records the path prefix each mount was asked for, so a test can
// assert the scope reached every mount rather than only that the page rendered.
type lensMotifsStub struct {
	clusters map[string][]store.MotifCluster
	defs     map[string]map[string]store.MotifDefinitionStatus
	health   map[string]store.MotifVocabularyHealth
	aliases  map[string]map[string]store.AliasRow
	errRepo  string

	lastPath       map[string]string
	lastHealthPath map[string]string
	lastKeys       map[string][]string
}

func (s *lensMotifsStub) Clusters(_ context.Context, ri *repos.RepoInstance, _, pathPrefix string) ([]store.MotifCluster, error) {
	if s.lastPath == nil {
		s.lastPath = map[string]string{}
	}
	s.lastPath[ri.Name()] = pathPrefix
	if s.errRepo != "" && ri.Name() == s.errRepo {
		return nil, errors.New("db on fire")
	}
	return s.clusters[ri.Name()], nil
}

func (s *lensMotifsStub) Definitions(_ context.Context, ri *repos.RepoInstance, _ string, keys []string) (map[string]store.MotifDefinitionStatus, error) {
	if s.lastKeys == nil {
		s.lastKeys = map[string][]string{}
	}
	s.lastKeys[ri.Name()] = keys
	if s.errRepo != "" && ri.Name() == s.errRepo {
		return nil, errors.New("db on fire")
	}
	return s.defs[ri.Name()], nil
}

func (s *lensMotifsStub) VocabularyHealth(_ context.Context, ri *repos.RepoInstance, _, pathPrefix string) (store.MotifVocabularyHealth, error) {
	if s.lastHealthPath == nil {
		s.lastHealthPath = map[string]string{}
	}
	s.lastHealthPath[ri.Name()] = pathPrefix
	if s.errRepo != "" && ri.Name() == s.errRepo {
		return store.MotifVocabularyHealth{}, errors.New("db on fire")
	}
	return s.health[ri.Name()], nil
}

func (s *lensMotifsStub) AliasRows(_ context.Context, ri *repos.RepoInstance, _ string) (map[string]store.AliasRow, error) {
	if s.errRepo != "" && ri.Name() == s.errRepo {
		return nil, errors.New("db on fire")
	}
	return s.aliases[ri.Name()], nil
}

func (s *lensMotifsStub) ClusterKey(_ context.Context, _ *repos.RepoInstance, _, motif string) (string, error) {
	return motif, nil
}

// lensMotifsServer wires a two-mount lens (write=alpha, read=beta) over the
// given stubs and returns the router plus the manager (for id12 lookups).
func lensMotifsServer(t *testing.T, motifs *lensMotifsStub, facts factsCollectionProvider) (http.Handler, *repos.Manager) {
	t.Helper()
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{motifs: motifs, factsCollection: facts}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	return r, m
}

// ─── collection ──────────────────────────────────────────────────────────────

// Two mounts, three kinds of overlap in one fixture:
//   - `drift-config` carries the SAME cluster key on both mounts → one row.
//   - beta's `degradation-quiet` shares no key with alpha's `fallback-silent`
//     but DOES share the spelling `quiet-degradation` (alpha's judge merged it
//     in) → still one row, because a spelling belongs to exactly one cluster.
//   - `creep-scope` exists on alpha only → passes through untouched.
func splitVocabularyStub() *lensMotifsStub {
	return &lensMotifsStub{
		clusters: map[string][]store.MotifCluster{
			"alpha": {
				{ClusterKey: "drift-config", CanonicalID: "config-drift", Members: []string{"config-drift"}, DF: 5, DFTotal: 9},
				{ClusterKey: "fallback-silent", CanonicalID: "silent-fallback", Members: []string{"quiet-degradation", "silent-fallback"}, DF: 4, DFTotal: 4},
				{ClusterKey: "creep-scope", CanonicalID: "scope-creep", Members: []string{"scope-creep"}, DF: 1, DFTotal: 1},
			},
			"beta": {
				{ClusterKey: "degradation-quiet", CanonicalID: "quiet-degradation", Members: []string{"quiet-degradation"}, DF: 3, DFTotal: 3},
				{ClusterKey: "drift-config", CanonicalID: "configuration-drifts", Members: []string{"configuration-drifts"}, DF: 2, DFTotal: 6},
			},
		},
	}
}

func TestLensMotifs_UnionMergesClustersAcrossMounts(t *testing.T) {
	r, _ := lensMotifsServer(t, splitVocabularyStub(), nil)
	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/eng/motifs"))

	if body.Count != 3 {
		t.Fatalf("count: got %d, want 3 merged clusters; body=%+v", body.Count, body.Embedded.Motifs)
	}
	byKey := map[string]int{}
	for i, e := range body.Embedded.Motifs {
		byKey[e.ClusterKey] = i
	}

	// Same key on both mounts: members union, df and df_total sum.
	i, ok := byKey["drift-config"]
	if !ok {
		t.Fatalf("no drift-config row; got %+v", body.Embedded.Motifs)
	}
	drift := body.Embedded.Motifs[i]
	if got := strings.Join(drift.Members, ","); got != "config-drift,configuration-drifts" {
		t.Errorf("drift members: got %q, want the sorted union", got)
	}
	if drift.DF != 7 || drift.DFTotal != 15 {
		t.Errorf("drift counts: got df=%d df_total=%d, want 7/15 (per-mount sums)", drift.DF, drift.DFTotal)
	}
	// canonical is the higher-DF constituent's representative (alpha's 5 > beta's 2).
	if drift.Canonical != "config-drift" {
		t.Errorf("drift canonical: got %q, want config-drift (highest-df constituent)", drift.Canonical)
	}

	// Different keys, shared spelling: still ONE row, keyed by the
	// lexicographically smallest constituent key.
	i, ok = byKey["degradation-quiet"]
	if !ok {
		t.Fatalf("no merged silent-fallback row keyed degradation-quiet; got %+v", body.Embedded.Motifs)
	}
	fallback := body.Embedded.Motifs[i]
	if got := strings.Join(fallback.Members, ","); got != "quiet-degradation,silent-fallback" {
		t.Errorf("fallback members: got %q, want the sorted union across the shared spelling", got)
	}
	if fallback.DF != 7 {
		t.Errorf("fallback df: got %d, want 7", fallback.DF)
	}
	if fallback.Canonical != "silent-fallback" {
		t.Errorf("fallback canonical: got %q, want silent-fallback (df 4 > 3)", fallback.Canonical)
	}
	// The spelling must appear in exactly ONE row — two rows carrying it would
	// pivot to different lists from names the reader cannot tell apart.
	rows := 0
	for _, e := range body.Embedded.Motifs {
		for _, m := range e.Members {
			if m == "quiet-degradation" {
				rows++
			}
		}
	}
	if rows != 1 {
		t.Errorf("quiet-degradation appears in %d rows, want exactly 1", rows)
	}

	// df-desc is the default order: drift 7, fallback 7 (canonical tiebreak
	// config-drift < silent-fallback), creep-scope 1.
	if body.Embedded.Motifs[len(body.Embedded.Motifs)-1].ClusterKey != "creep-scope" {
		t.Errorf("order: got %v, want df-desc with creep-scope last",
			[]string{body.Embedded.Motifs[0].ClusterKey, body.Embedded.Motifs[1].ClusterKey, body.Embedded.Motifs[2].ClusterKey})
	}

	if got := getLensFacts(t, r, "/lenses/eng/motifs").Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
}

// Self links address the LENS, not a mount: a row's self link must be openable
// as /lenses/{lens}/motifs/{key}.
func TestLensMotifs_LinksAddressTheLens(t *testing.T) {
	r, _ := lensMotifsServer(t, splitVocabularyStub(), nil)
	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/eng/motifs?sort=name"))

	if !strings.HasSuffix(body.Links.Self.Href, "/lenses/eng/motifs?sort=name") {
		t.Errorf("collection self: got %q", body.Links.Self.Href)
	}
	for _, e := range body.Embedded.Motifs {
		want := "/lenses/eng/motifs/" + e.ClusterKey
		if !strings.HasSuffix(e.Links.Self.Href, want) {
			t.Errorf("entry self: got %q, want a href ending %q", e.Links.Self.Href, want)
		}
	}
}

// Health counts are POOLED across mounts and the two ratios are derived ONCE
// from the pooled totals — never a mean of per-mount ratios.
func TestLensMotifs_HealthPoolsCountsAndDerivesRatiosOnce(t *testing.T) {
	stub := splitVocabularyStub()
	stub.health = map[string]store.MotifVocabularyHealth{
		"alpha": {Clusters: 10, Recurring: 2, Mints: 8, Links: 4, EpistemicRecurring: 1},
		"beta":  {Clusters: 10, Recurring: 8, Mints: 2, Links: 16, EpistemicRecurring: 3},
	}
	r, _ := lensMotifsServer(t, stub, nil)
	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/eng/motifs"))

	h := body.Health
	if h.AuthoredClusters != 20 || h.AuthoredRecurring != 10 || h.AuthoredMints != 10 || h.AuthoredLinks != 20 {
		t.Fatalf("pooled counts: got %+v", h)
	}
	if h.AuthoredEpistemicRecurring != 4 {
		t.Errorf("authored_epistemic_recurring: got %d, want 4", h.AuthoredEpistemicRecurring)
	}
	pooled := store.MotifVocabularyHealth{Clusters: 20, Recurring: 10, Mints: 10, Links: 20, EpistemicRecurring: 4}
	if h.RecurrenceRate != pooled.RecurrenceRate() {
		t.Errorf("recurrence_rate: got %v, want %v (the rule applied ONCE to pooled totals)",
			h.RecurrenceRate, pooled.RecurrenceRate())
	}
	// The per-mount ratios are 8/4=2 and 2/16=0.125; their mean (1.0625) is
	// NOT the pooled answer (10/20=0.5). This is the assertion that fails if
	// the handler averages ratios instead of pooling counts.
	if h.MintToLinkRatio != pooled.MintToLinkRatio() {
		t.Errorf("mint_to_link_ratio: got %v, want %v (pooled, not a mean of ratios)",
			h.MintToLinkRatio, pooled.MintToLinkRatio())
	}
}

// ?path= is SCOPE: it reaches every mount, and it narrows the health block as
// well as the list — exactly as it does on the repo endpoint.
func TestLensMotifs_PathScopesEveryMountAndItsHealth(t *testing.T) {
	stub := splitVocabularyStub()
	r, _ := lensMotifsServer(t, stub, nil)
	// A topic the test repos' ontology preset actually has, so the fan-out's
	// ontology-aware skip does not quietly drop both mounts and turn this into
	// an assertion about nothing.
	getLensFacts(t, r, "/lenses/eng/motifs?path=kb/technology/")

	for _, name := range []string{"alpha", "beta"} {
		if stub.lastPath[name] != "kb/technology/" {
			t.Errorf("%s clusters path: got %q, want kb/technology/", name, stub.lastPath[name])
		}
		if stub.lastHealthPath[name] != "kb/technology/" {
			t.Errorf("%s health path: got %q, want kb/technology/", name, stub.lastHealthPath[name])
		}
	}
}

// repo=<name> narrows the fan-out; an unknown name is a 422 (the shared lens
// union-read filter).
func TestLensMotifs_RepoFilterNarrows(t *testing.T) {
	r, _ := lensMotifsServer(t, splitVocabularyStub(), nil)
	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/eng/motifs?repo=beta"))
	if body.Count != 2 {
		t.Fatalf("count: got %d, want beta's 2 clusters; %+v", body.Count, body.Embedded.Motifs)
	}
	for _, e := range body.Embedded.Motifs {
		if e.ClusterKey == "creep-scope" {
			t.Errorf("alpha-only cluster leaked through repo=beta: %+v", e)
		}
	}
}

func TestLensMotifs_UnknownRepoFilterIs422(t *testing.T) {
	r, _ := lensMotifsServer(t, splitVocabularyStub(), nil)
	rec := getLensFacts(t, r, "/lenses/eng/motifs?repo=nope")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// A lens never silently shrinks its read set: one mount failing fails the
// WHOLE request (RFC §9.1), rather than returning the surviving mount's
// vocabulary as if it were the union's.
func TestLensMotifs_MountErrorFailsTheWholeRequest(t *testing.T) {
	stub := splitVocabularyStub()
	stub.errRepo = "beta"
	r, _ := lensMotifsServer(t, stub, nil)
	rec := getLensFacts(t, r, "/lenses/eng/motifs")
	if rec.Code == http.StatusOK {
		t.Fatalf("a failing mount must not produce 200; body=%s", rec.Body.String())
	}
}

// ?q= narrows the list over member spellings AND definition text, and — unlike
// ?path= — leaves the health block alone.
func TestLensMotifs_QNarrowsListAcrossMountsButNotHealth(t *testing.T) {
	stub := splitVocabularyStub()
	stub.health = map[string]store.MotifVocabularyHealth{
		"alpha": {Clusters: 3, Recurring: 1, Mints: 2, Links: 2},
	}
	stub.defs = map[string]map[string]store.MotifDefinitionStatus{
		// The definition lives on the READ mount; a handler that only looks
		// definitions up on the write repo would miss this match.
		"beta": {"drift-config": {Definition: "applied state diverges"}},
	}
	r, _ := lensMotifsServer(t, stub, nil)
	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/eng/motifs?q=diverges"))

	if body.Count != 1 || body.Embedded.Motifs[0].ClusterKey != "drift-config" {
		t.Fatalf("q over definitions: got %d rows %+v", body.Count, body.Embedded.Motifs)
	}
	if body.Health.AuthoredClusters != 3 {
		t.Errorf("health must ignore q: got %d, want 3", body.Health.AuthoredClusters)
	}
}

// The definition shown for a merged cluster is the freshest one across mounts:
// a `current` definition beats a `stale` one wherever it lives.
func TestLensMotifs_FreshestDefinitionWins(t *testing.T) {
	stub := splitVocabularyStub()
	stub.defs = map[string]map[string]store.MotifDefinitionStatus{
		"alpha": {"drift-config": {Definition: "stale wording", Stale: true}},
		"beta":  {"drift-config": {Definition: "current wording"}},
	}
	r, _ := lensMotifsServer(t, stub, nil)
	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/eng/motifs?q=wording"))
	if len(body.Embedded.Motifs) != 1 {
		t.Fatalf("got %+v", body.Embedded.Motifs)
	}
	got := body.Embedded.Motifs[0]
	if got.Definition != "current wording" || got.DefinitionState != "current" {
		t.Errorf("definition: got %q/%q, want the current one from the read mount",
			got.Definition, got.DefinitionState)
	}
}

// Paging is by cluster over the MERGED vocabulary, and `count` is the number of
// merged clusters the query matches — never the number of rows transferred.
func TestLensMotifs_PagingCountsMatchesNotRows(t *testing.T) {
	r, _ := lensMotifsServer(t, splitVocabularyStub(), nil)
	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/eng/motifs?limit=1"))
	if body.Count != 3 {
		t.Errorf("count: got %d, want 3 (matches, not the 1 row fetched)", body.Count)
	}
	if len(body.Embedded.Motifs) != 1 {
		t.Errorf("page: got %d rows, want 1", len(body.Embedded.Motifs))
	}
	if body.Links.Next == nil || !strings.Contains(body.Links.Next.Href, "offset=1") {
		t.Errorf("next link: got %+v", body.Links.Next)
	}
}

func TestLensMotifs_LimitOverTheCeilingIs400(t *testing.T) {
	r, _ := lensMotifsServer(t, splitVocabularyStub(), nil)
	rec := getLensFacts(t, r, "/lenses/eng/motifs?limit=99999")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (hard ceiling, never a quiet clamp)", rec.Code)
	}
}

// A lens over ONE repo answers with that repo's vocabulary unchanged — same
// keys, same canonicals, same counts. The merge must be an identity there.
func TestLensMotifs_LensOfOneIsTheRepoVocabulary(t *testing.T) {
	stub := &lensMotifsStub{
		clusters: map[string][]store.MotifCluster{"alpha": threeClusters()},
		health:   map[string]store.MotifVocabularyHealth{"alpha": {Clusters: 3, Recurring: 1, Mints: 2, Links: 5}},
	}
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{motifs: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"solo","write":"alpha","reads":[]}`)

	body := decodeMotifs(t, getLensFacts(t, r, "/lenses/solo/motifs"))
	want := threeClusters()
	if body.Count != len(want) {
		t.Fatalf("count: got %d, want %d", body.Count, len(want))
	}
	// Keyed, not indexed: the union emits the store's own order (df-desc,
	// canonical-asc), and threeClusters is a hand-written fixture that is not
	// in it. What "identity" means here is that every cluster survives with its
	// key, its representative and its counts untouched.
	got := map[string]struct {
		canonical string
		df        int
	}{}
	for _, e := range body.Embedded.Motifs {
		got[e.ClusterKey] = struct {
			canonical string
			df        int
		}{e.Canonical, e.DF}
	}
	for _, c := range want {
		if g, ok := got[c.ClusterKey]; !ok || g.canonical != c.CanonicalID || g.df != c.DF {
			t.Errorf("cluster %q: got %+v, want canonical=%q df=%d", c.ClusterKey, g, c.CanonicalID, c.DF)
		}
	}
	// df-desc: the 5 comes before the two 2s.
	if body.Embedded.Motifs[0].ClusterKey != "drift-config" {
		t.Errorf("order: got %q first, want the df-5 cluster", body.Embedded.Motifs[0].ClusterKey)
	}
	if body.Health.AuthoredClusters != 3 || body.Health.AuthoredLinks != 5 {
		t.Errorf("health: got %+v, want the single mount's own numbers", body.Health)
	}
}

// ─── cluster detail ──────────────────────────────────────────────────────────

// lensCarriersStub is a per-repo factsCollectionProvider for the carrier
// preview, recording the SearchOptions each mount was asked for.
type lensCarriersStub struct {
	byRepo      map[string][]store.RecentFactEntry
	totalByRepo map[string]int
	lastOpts    map[string]store.SearchOptions
}

func (s *lensCarriersStub) RecentFacts(
	_ context.Context, ri *repos.RepoInstance, _ string, opts store.SearchOptions,
) ([]store.RecentFactEntry, int, error) {
	if s.lastOpts == nil {
		s.lastOpts = map[string]store.SearchOptions{}
	}
	s.lastOpts[ri.Name()] = opts
	e := s.byRepo[ri.Name()]
	total := len(e)
	if n, ok := s.totalByRepo[ri.Name()]; ok {
		total = n
	}
	if opts.Limit > 0 && len(e) > opts.Limit {
		e = e[:opts.Limit]
	}
	return e, total, nil
}

func TestLensMotifCluster_ResolvesByMergedKeyConstituentKeyAndSpelling(t *testing.T) {
	carriers := &lensCarriersStub{byRepo: map[string][]store.RecentFactEntry{}}
	r, _ := lensMotifsServer(t, splitVocabularyStub(), carriers)

	// The merged key, the OTHER mount's constituent key, and a member spelling
	// all address the same merged cluster.
	for _, key := range []string{"degradation-quiet", "fallback-silent", "silent-fallback", "quiet-degradation"} {
		body := decodeMotifDetail(t, getLensFacts(t, r, "/lenses/eng/motifs/"+key))
		if body.ClusterKey != "degradation-quiet" {
			t.Errorf("%q resolved to %q, want the merged key degradation-quiet", key, body.ClusterKey)
		}
		if got := strings.Join(body.Members, ","); got != "quiet-degradation,silent-fallback" {
			t.Errorf("%q members: got %q", key, got)
		}
		if !strings.HasSuffix(body.Links.Self.Href, "/lenses/eng/motifs/degradation-quiet") {
			t.Errorf("%q self link: got %q, want the merged key", key, body.Links.Self.Href)
		}
	}
}

func TestLensMotifCluster_UnknownIs404(t *testing.T) {
	r, _ := lensMotifsServer(t, splitVocabularyStub(), &lensCarriersStub{})
	rec := getLensFacts(t, r, "/lenses/eng/motifs/nothing-like-this")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// The pivot link is the lens's own /facts filtered by the MERGED member list —
// so every mount expands the union against its own alias table, and the preview
// and the full listing cannot disagree.
func TestLensMotifCluster_PivotLinkCarriesTheMergedMembers(t *testing.T) {
	carriers := &lensCarriersStub{}
	r, _ := lensMotifsServer(t, splitVocabularyStub(), carriers)
	body := decodeMotifDetail(t, getLensFacts(t, r, "/lenses/eng/motifs/degradation-quiet"))

	href := body.Links.Facts.Href
	if !strings.Contains(href, "/lenses/eng/facts?") {
		t.Fatalf("facts link must point at the lens: got %q", href)
	}
	if !strings.Contains(href, "motifs=quiet-degradation%2Csilent-fallback") ||
		!strings.Contains(href, "motif_match=exact") {
		t.Errorf("facts link: got %q, want the merged members at the exact tier", href)
	}
	// Every mount was asked with the same union filter.
	for _, name := range []string{"alpha", "beta"} {
		opts := carriers.lastOpts[name]
		if strings.Join(opts.Motifs, ",") != "quiet-degradation,silent-fallback" {
			t.Errorf("%s carrier query motifs: got %v, want the merged union", name, opts.Motifs)
		}
		if opts.MotifMatch != store.MotifMatchExact {
			t.Errorf("%s carrier query tier: got %q, want exact", name, opts.MotifMatch)
		}
	}
}

// TestLensMotifCluster_CarriersCarryQualifiedPaths is the lens-motif twin of
// TestLensStats_HighlightsCarryQualifiedPaths: every path-bearing row a lens
// read emits must carry the canonical wire path — bare for the write mount,
// kb://<id12>/… for a read mount — or opening it 404s against the write repo.
func TestLensMotifCluster_CarriersCarryQualifiedPaths(t *testing.T) {
	carriers := &lensCarriersStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/from-write.md", Title: "write", Type: "observation", CommittedAt: 200}},
		"beta":  {{Path: "kb/from-read.md", Title: "read", Type: "observation", CommittedAt: 100}},
	}}
	r, m := lensMotifsServer(t, splitVocabularyStub(), carriers)
	body := decodeMotifDetail(t, getLensFacts(t, r, "/lenses/eng/motifs/drift-config"))

	byTitle := map[string]string{}
	for _, c := range body.Carriers {
		byTitle[c.Title] = c.Path
	}
	if got := byTitle["write"]; got != "kb/from-write.md" {
		t.Errorf("write mount carrier: got %q, want the bare path", got)
	}
	want := federate.QualifyPath(federate.ID12(m.Get("beta").ID()), "kb/from-read.md")
	if got := byTitle["read"]; got != want {
		t.Errorf("read mount carrier: got %q, want %q", got, want)
	}
	// Recency order across mounts, newest first.
	if len(body.Carriers) != 2 || body.Carriers[0].Title != "write" {
		t.Errorf("carriers: got %+v, want newest-first across mounts", body.Carriers)
	}
}

// Qualification happens AFTER the dedupe: a re-rooted fork mounted beside its
// upstream shares fact UUIDs, so the same repo-relative path arrives from two
// mounts and the WRITE mount's copy must win exactly once.
func TestLensMotifCluster_CarrierDedupeKeepsTheWriteCopy(t *testing.T) {
	carriers := &lensCarriersStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/dup.md", Title: "from write", Type: "observation", CommittedAt: 100}},
		"beta":  {{Path: "kb/dup.md", Title: "from read", Type: "observation", CommittedAt: 300}},
	}}
	r, _ := lensMotifsServer(t, splitVocabularyStub(), carriers)
	body := decodeMotifDetail(t, getLensFacts(t, r, "/lenses/eng/motifs/drift-config"))

	if len(body.Carriers) != 1 {
		t.Fatalf("carriers: got %d rows, want 1 deduped; %+v", len(body.Carriers), body.Carriers)
	}
	if body.Carriers[0].Title != "from write" || body.Carriers[0].Path != "kb/dup.md" {
		t.Errorf("carrier: got %+v, want the write mount's bare-path copy", body.Carriers[0])
	}
	if body.CarrierCount != 1 {
		t.Errorf("carrier_count: got %d, want 1 — the union holds one fact", body.CarrierCount)
	}
}

// carrier_count counts MATCHES, never fetched rows: bounding the preview must
// never bound the count (kb/invariants/web/collections/count-vs-transfer).
func TestLensMotifCluster_CarrierCountIsNotBoundedByThePreview(t *testing.T) {
	carriers := &lensCarriersStub{
		byRepo: map[string][]store.RecentFactEntry{
			"alpha": {{Path: "kb/a.md", Title: "a", CommittedAt: 200}},
			"beta":  {{Path: "kb/b.md", Title: "b", CommittedAt: 100}},
		},
		// Both mounts hold far more carriers than the preview fetched.
		totalByRepo: map[string]int{"alpha": 40, "beta": 9},
	}
	r, _ := lensMotifsServer(t, splitVocabularyStub(), carriers)
	body := decodeMotifDetail(t, getLensFacts(t, r, "/lenses/eng/motifs/drift-config?limit=1"))

	if len(body.Carriers) > 1 {
		t.Errorf("preview: got %d rows, want at most the requested 1", len(body.Carriers))
	}
	if body.CarrierCount != 49 {
		t.Errorf("carrier_count: got %d, want 49 (the summed per-mount match counts)", body.CarrierCount)
	}
}

// Alias rows merge across mounts, write mount first — the audit trail for a
// spelling comes from the mount that actually recorded a decision about it.
func TestLensMotifCluster_AliasesMergeWriteFirst(t *testing.T) {
	stub := splitVocabularyStub()
	stub.aliases = map[string]map[string]store.AliasRow{
		"alpha": {
			"config-drift": {ClusterKey: "drift-config", CanonicalID: "config-drift", Method: "mechanical"},
		},
		"beta": {
			"configuration-drifts": {ClusterKey: "drift-config", CanonicalID: "configuration-drifts", Method: "judge", Rationale: "same mechanism"},
		},
	}
	r, _ := lensMotifsServer(t, stub, &lensCarriersStub{})
	body := decodeMotifDetail(t, getLensFacts(t, r, "/lenses/eng/motifs/drift-config"))

	byMotif := map[string]struct{ method, rationale string }{}
	for _, a := range body.Aliases {
		byMotif[a.Motif] = struct{ method, rationale string }{a.Method, a.Rationale}
	}
	if len(body.Aliases) != 2 {
		t.Fatalf("aliases: got %+v, want one row per merged member", body.Aliases)
	}
	if byMotif["config-drift"].method != "mechanical" {
		t.Errorf("write-mount alias row lost its method: %+v", byMotif["config-drift"])
	}
	if byMotif["configuration-drifts"].method != "judge" ||
		byMotif["configuration-drifts"].rationale != "same mechanism" {
		t.Errorf("read-mount alias row lost its audit trail: %+v", byMotif["configuration-drifts"])
	}
}

func TestLensMotifCluster_MountErrorFailsTheWholeRequest(t *testing.T) {
	stub := splitVocabularyStub()
	stub.errRepo = "beta"
	r, _ := lensMotifsServer(t, stub, &lensCarriersStub{})
	rec := getLensFacts(t, r, "/lenses/eng/motifs/drift-config")
	if rec.Code == http.StatusOK {
		t.Fatalf("a failing mount must not produce 200; body=%s", rec.Body.String())
	}
}

// The detail is addressed by IDENTITY, so it is branch-wide on every mount: no
// path scope reaches the fan-out, exactly as the repo detail passes "".
func TestLensMotifCluster_IsBranchWideOnEveryMount(t *testing.T) {
	stub := splitVocabularyStub()
	r, _ := lensMotifsServer(t, stub, &lensCarriersStub{})
	getLensFacts(t, r, "/lenses/eng/motifs/drift-config?path=kb/technology/")
	for _, name := range []string{"alpha", "beta"} {
		if stub.lastPath[name] != "" {
			t.Errorf("%s: got path %q, want branch-wide (\"\")", name, stub.lastPath[name])
		}
	}
}
