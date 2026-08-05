package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// lensStatsStub is a per-repo statsProvider: each mount's Stats call is
// answered from byRepo keyed by the repo's name, so a single stub drives the
// whole fan-out. It records the last pathPrefix and axis seen per repo so
// tests can assert per-target forwarding; errRepo makes that one mount fail.
//
// byRepoAxis is an optional repo -> requested-axis -> result override, tried
// before byRepo. It exists to simulate what a real store.Stats does: cut a
// DIFFERENT top-N candidate set depending on the requested axis (store's SQL
// LIMIT is ranked by that axis). Without it every axis value would echo the
// same canned Highlights, which cannot distinguish a correct re-fan-out at a
// resolved axis from a stale first-pass result cut by the wrong one.
type lensStatsStub struct {
	byRepo     map[string]store.StatsResult
	byRepoAxis map[string]map[string]store.StatsResult
	errRepo    string
	lastPath   map[string]string
	lastAxis   map[string]string
}

func (s *lensStatsStub) Stats(_ context.Context, ri *repos.RepoInstance, _ string, pathPrefix, axis string) (store.StatsResult, error) {
	if s.lastPath == nil {
		s.lastPath = map[string]string{}
	}
	if s.lastAxis == nil {
		s.lastAxis = map[string]string{}
	}
	name := ri.Name()
	s.lastPath[name] = pathPrefix
	s.lastAxis[name] = axis
	if s.errRepo != "" && name == s.errRepo {
		return store.StatsResult{}, errors.New("db on fire")
	}
	if perAxis, ok := s.byRepoAxis[name]; ok {
		if res, ok := perAxis[axis]; ok {
			return res, nil
		}
	}
	return s.byRepo[name], nil
}

// lensActivityStub is the per-repo activityProvider twin of lensStatsStub.
type lensActivityStub struct {
	byRepo   map[string]store.ActivityResult
	errRepo  string
	lastPath map[string]string
}

func (s *lensActivityStub) Activity(_ context.Context, ri *repos.RepoInstance, _ string, path string) (store.ActivityResult, error) {
	if s.lastPath == nil {
		s.lastPath = map[string]string{}
	}
	s.lastPath[ri.Name()] = path
	if s.errRepo != "" && ri.Name() == s.errRepo {
		return store.ActivityResult{}, errors.New("git on fire")
	}
	return s.byRepo[ri.Name()], nil
}

// lensStatsBody mirrors the union stats wire shape.
type lensStatsBody struct {
	Total         int            `json:"total"`
	RepoCount     int            `json:"repo_count"`
	LastCommit    string         `json:"last_commit"`
	AvgConfidence float64        `json:"avg_confidence"`
	Domains       map[string]int `json:"domains"`
	Entities      map[string]int `json:"entities"`
	Types         map[string]int `json:"types"`
	Highlights    []struct {
		Path       string  `json:"path"`
		Title      string  `json:"title"`
		Impact     int     `json:"impact"`
		Confidence float64 `json:"confidence"`
	} `json:"highlights"`
	DefaultAxis string `json:"default_axis"`
	Repos       []struct {
		ID            string         `json:"id"`
		Name          string         `json:"name"`
		Source        string         `json:"source"`
		Branch        string         `json:"branch"`
		IsWrite       bool           `json:"is_write"`
		Total         int            `json:"total"`
		AvgConfidence float64        `json:"avg_confidence"`
		Domains       map[string]int `json:"domains"`
		Entities      map[string]int `json:"entities"`
		LastCommit    string         `json:"last_commit"`
		Changes7d     int            `json:"changes_7d"`
		Changes30d    int            `json:"changes_30d"`
		Changes90d    int            `json:"changes_90d"`
	} `json:"repos"`
}

func decodeLensStats(t *testing.T, rec *httptest.ResponseRecorder) lensStatsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body lensStatsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body
}

// The union roll-up sums totals/domains/entities across mounts, weights
// avg_confidence by per-mount total, takes the MAX last_commit, and lists one
// per-repo row per fanned-out target — is_write set on the write repo only,
// id = the mount's id12, branch = the Binding-resolved read branch.
func TestLensStats_UnionAggregates(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	statsStub := &lensStatsStub{byRepo: map[string]store.StatsResult{
		"alpha": {Total: 200, AvgConfidence: 0.9, Domains: map[string]int{"go": 5, "ai": 10}, Entities: map[string]int{"chi": 3}, Types: map[string]int{"synthesis": 4, "observation": 3}},
		"beta":  {Total: 50, AvgConfidence: 0.5, Domains: map[string]int{"go": 2, "web": 1}, Entities: map[string]int{"vite": 4}, Types: map[string]int{"synthesis": 1}},
	}}
	actStub := &lensActivityStub{byRepo: map[string]store.ActivityResult{
		"alpha": {LastCommit: "2026-07-19T09:00:00Z", Total: 40, Changes7d: 1, Changes30d: 2, Changes90d: 3},
		"beta":  {LastCommit: "2026-07-20T10:00:00Z", Total: 7, Changes7d: 4, Changes30d: 5, Changes90d: 6},
	}}
	s := &Server{Manager: m, providers: storeProviders{stats: statsStub, activity: actStub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/stats")
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	body := decodeLensStats(t, rec)

	if body.Total != 250 {
		t.Errorf("total: got %d, want 250 (sum across mounts)", body.Total)
	}
	if body.RepoCount != 2 {
		t.Errorf("repo_count: got %d, want 2", body.RepoCount)
	}
	// Total-weighted mean: (0.9*200 + 0.5*50) / 250 = 0.82.
	if diff := body.AvgConfidence - 0.82; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("avg_confidence: got %v, want 0.82 (total-weighted mean)", body.AvgConfidence)
	}
	if body.LastCommit != "2026-07-20T10:00:00Z" {
		t.Errorf("last_commit: got %q, want the MAX across mounts", body.LastCommit)
	}
	if body.Domains["go"] != 7 || body.Domains["ai"] != 10 || body.Domains["web"] != 1 {
		t.Errorf("domains: got %v, want merged sums {go:7 ai:10 web:1}", body.Domains)
	}
	if body.Entities["chi"] != 3 || body.Entities["vite"] != 4 {
		t.Errorf("entities: got %v, want merged sums {chi:3 vite:4}", body.Entities)
	}
	if body.Types["synthesis"] != 5 || body.Types["observation"] != 3 {
		t.Errorf("types: got %v, want merged sums {synthesis:5 observation:3}", body.Types)
	}
	if len(body.Repos) != 2 {
		t.Fatalf("repos: got %d rows, want 2; body=%+v", len(body.Repos), body)
	}
	// Reads() is sorted by repo name → alpha (write) then beta.
	alpha, beta := body.Repos[0], body.Repos[1]
	if alpha.Name != "alpha" || !alpha.IsWrite {
		t.Errorf("row 0: got %+v, want name=alpha is_write=true", alpha)
	}
	if beta.Name != "beta" || beta.IsWrite {
		t.Errorf("row 1: got %+v, want name=beta is_write=false", beta)
	}
	alphaRI, betaRI := m.Get("alpha"), m.Get("beta")
	if alpha.ID != federate.ID12(alphaRI.ID()) || beta.ID != federate.ID12(betaRI.ID()) {
		t.Errorf("ids: got %q/%q, want the mounts' id12s", alpha.ID, beta.ID)
	}
	if alpha.Branch != alphaRI.AgentBranch() || beta.Branch != betaRI.AgentBranch() {
		t.Errorf("branches: got %q/%q, want the resolved read branches", alpha.Branch, beta.Branch)
	}
	if beta.Total != 50 || beta.AvgConfidence != 0.5 || beta.LastCommit != "2026-07-20T10:00:00Z" ||
		beta.Changes7d != 4 || beta.Changes30d != 5 || beta.Changes90d != 6 {
		t.Errorf("beta row must carry its own stats+activity, got %+v", beta)
	}
}

// repo=<name> narrows the fan-out to the named mount(s): the roll-up and
// repo_count reflect only those mounts.
func TestLensStats_RepoFilterNarrows(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	statsStub := &lensStatsStub{byRepo: map[string]store.StatsResult{
		"alpha": {Total: 200, AvgConfidence: 0.9},
		"beta":  {Total: 50, AvgConfidence: 0.5},
	}}
	actStub := &lensActivityStub{byRepo: map[string]store.ActivityResult{
		"beta": {LastCommit: "2026-07-20T10:00:00Z"},
	}}
	s := &Server{Manager: m, providers: storeProviders{stats: statsStub, activity: actStub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensStats(t, getLensFacts(t, r, "/lenses/eng/stats?repo=beta"))
	if body.RepoCount != 1 {
		t.Errorf("repo_count: got %d, want 1 (narrowed)", body.RepoCount)
	}
	if body.Total != 50 {
		t.Errorf("total: got %d, want 50 (beta only)", body.Total)
	}
	if len(body.Repos) != 1 || body.Repos[0].Name != "beta" {
		t.Fatalf("repos: got %+v, want the single beta row", body.Repos)
	}
}

// An unknown repo= name is a well-formed request naming a nonexistent mount → 422.
func TestLensStats_UnknownRepoFilter422(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{stats: &lensStatsStub{}, activity: &lensActivityStub{}}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/stats?repo=ghost")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// An empty lens (no facts, no commits on any mount) yields zero totals,
// avg_confidence 0 (no division by zero), last_commit "", NON-NULL maps at
// both the union and per-repo levels, and a per-repo row per mount.
func TestLensStats_EmptyMounts(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{stats: &lensStatsStub{}, activity: &lensActivityStub{}}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensStats(t, getLensFacts(t, r, "/lenses/eng/stats"))
	if body.Total != 0 || body.AvgConfidence != 0 {
		t.Errorf("empty union: got total=%d avg=%v, want 0/0", body.Total, body.AvgConfidence)
	}
	if body.LastCommit != "" {
		t.Errorf("last_commit: got %q, want \"\"", body.LastCommit)
	}
	if body.Domains == nil || body.Entities == nil || body.Types == nil {
		t.Error("union domains/entities/types must be {} not null")
	}
	if len(body.Repos) != 2 {
		t.Fatalf("repos: got %d rows, want 2 (mounts still listed)", len(body.Repos))
	}
	for _, row := range body.Repos {
		if row.Domains == nil || row.Entities == nil {
			t.Errorf("row %s: domains/entities must be {} not null", row.Name)
		}
	}
}

// An empty lens's types union serializes as the literal `{}`, not `null` —
// exact-string check on the body, mirroring
// TestHandleHALStats_EmptyHighlightsSerializeAsArrayNotNull on the repo
// endpoint (handlers_stats_test.go). The typed-body assertions above only
// prove a Go nil map decodes as non-nil; encoding/json would happily accept
// either `{}` or `null` there, so this is the only check that actually pins
// the wire representation.
func TestLensStats_EmptyMountsTypesSerializeAsObjectNotNull(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{stats: &lensStatsStub{}, activity: &lensActivityStub{}}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/stats")
	got := rec.Body.String()
	if !strings.Contains(got, `"types":{}`) {
		t.Errorf("types must serialize as {}, got: %s", got)
	}
}

// Any mount error fails the WHOLE request (RFC §9.1) — even when the failing
// mount is a read mount and the write repo answered fine — for BOTH the stats
// and the activity call.
func TestLensStats_MountErrorFailsWholeRequest(t *testing.T) {
	for name, providers := range map[string]struct {
		stats *lensStatsStub
		act   *lensActivityStub
	}{
		"stats error":    {&lensStatsStub{errRepo: "beta"}, &lensActivityStub{}},
		"activity error": {&lensStatsStub{}, &lensActivityStub{errRepo: "beta"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestLensManager(t, "alpha", "beta")
			s := &Server{Manager: m, providers: storeProviders{stats: providers.stats, activity: providers.act}}
			r := s.NewAPIRouter()
			createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

			rec := getLensFacts(t, r, "/lenses/eng/stats")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("content-type: got %q, want application/problem+json", got)
			}
		})
	}
}

// path= is forwarded to EVERY mount's stats AND activity providers as the
// per-target (repo-relative) prefix.
func TestLensStats_ForwardsPathPrefix(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	statsStub := &lensStatsStub{}
	actStub := &lensActivityStub{}
	s := &Server{Manager: m, providers: storeProviders{stats: statsStub, activity: actStub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	decodeLensStats(t, getLensFacts(t, r, "/lenses/eng/stats?path=kb/meta"))
	for _, repo := range []string{"alpha", "beta"} {
		if got := statsStub.lastPath[repo]; got != "kb/meta" {
			t.Errorf("stats path for %s: got %q, want kb/meta", repo, got)
		}
		if got := actStub.lastPath[repo]; got != "kb/meta" {
			t.Errorf("activity path for %s: got %q, want kb/meta", repo, got)
		}
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensStats_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, providers: storeProviders{stats: &lensStatsStub{}, activity: &lensActivityStub{}}}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/stats")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// getLensStatsBody drives GET /lenses/eng/stats over a two-mount lens
// (alpha = write, beta = read) with per-mount stats supplied by the caller.
// Reuses the existing newTestLensManager/lensStatsStub/createLens/
// getLensFacts/decodeLensStats fixture — no new scaffolding.
func getLensStatsBody(t *testing.T, byRepo map[string]store.StatsResult) lensStatsBody {
	t.Helper()
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{
		stats:    &lensStatsStub{byRepo: byRepo},
		activity: &lensActivityStub{byRepo: map[string]store.ActivityResult{}},
	}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	return decodeLensStats(t, getLensFacts(t, r, "/lenses/eng/stats"))
}

// TestLensStats_HighlightsAreGlobalTopNotFirstMount: mount B holds the highest
// impact fact; it must lead even though mount A is returned first.
//
// TopLayerFacts/TopLayerEdges are set (ObservationFacts left at 0) purely so
// the pooled default_axis resolves to impact, matching what this fixture's
// DefaultAxis fields already say per mount — this test is about merge order,
// not axis pooling; see TestLensStats_DefaultAxis* for that.
func TestLensStats_HighlightsAreGlobalTopNotFirstMount(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		"alpha": {
			Total: 1, DefaultAxis: "impact",
			TopLayerFacts: 1, TopLayerEdges: 5,
			Highlights: []store.Highlight{
				{Path: "kb/a.md", Title: "low", Type: "synthesis", Impact: 2},
			},
		},
		"beta": {
			Total: 1, DefaultAxis: "impact",
			TopLayerFacts: 1, TopLayerEdges: 5,
			Highlights: []store.Highlight{
				{Path: "kb/b.md", Title: "high", Type: "synthesis", Impact: 9},
			},
		},
	}
	body := getLensStatsBody(t, byRepo)

	if len(body.Highlights) != 2 {
		t.Fatalf("highlights: got %d, want 2", len(body.Highlights))
	}
	if body.Highlights[0].Path != "kb/b.md" {
		t.Errorf("top: got %q, want kb/b.md (impact 9)", body.Highlights[0].Path)
	}
}

// TestLensStats_HighlightsDedupeAcrossMounts: a re-rooted fork mounted beside
// its upstream shares fact UUIDs, so the same path can arrive twice. Unlike
// the aggregate sums, highlights carry paths and MUST dedupe — write mount
// wins, matching federate.WriteFirstWinners (the facts/search/topics unions'
// shared dedup). The two copies carry DIFFERENT Title/Confidence so the
// test can tell WHICH copy survived, not just that dedup happened at all.
func TestLensStats_HighlightsDedupeAcrossMounts(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		"alpha": { // write mount — must win
			Total: 1, DefaultAxis: "impact",
			Highlights: []store.Highlight{
				{Path: "kb/dup.md", Title: "from write (alpha)", Type: "synthesis", Impact: 5, Confidence: 0.9},
			},
		},
		"beta": { // read mount — must lose
			Total: 1, DefaultAxis: "impact",
			Highlights: []store.Highlight{
				{Path: "kb/dup.md", Title: "from read (beta)", Type: "synthesis", Impact: 5, Confidence: 0.2},
			},
		},
	}
	body := getLensStatsBody(t, byRepo)

	if len(body.Highlights) != 1 {
		t.Fatalf("highlights: got %d, want 1 after dedupe", len(body.Highlights))
	}
	if got := body.Highlights[0].Title; got != "from write (alpha)" {
		t.Errorf("dedupe winner: got title %q, want the write mount's copy (\"from write (alpha)\")", got)
	}
	if got := body.Highlights[0].Confidence; got != 0.9 {
		t.Errorf("dedupe winner: got confidence %v, want the write mount's copy (0.9)", got)
	}
}

// TestLensStats_HighlightsDedupeWriteWinsEvenWhenWriteSortsAfterRead:
// repos.Lens.normalize sorts Reads alphabetically by repo name, so the write
// mount is NOT necessarily first in fan-out order — a "first occurrence in
// iteration order wins" dedup would pick the read mount's copy here, which
// is wrong. Write must win regardless of where it sorts.
func TestLensStats_HighlightsDedupeWriteWinsEvenWhenWriteSortsAfterRead(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha")
	byRepo := map[string]store.StatsResult{
		"zulu": { // write mount, sorts AFTER "alpha" alphabetically
			Total: 1, DefaultAxis: "impact",
			Highlights: []store.Highlight{
				{Path: "kb/dup.md", Title: "from write (zulu)", Type: "synthesis", Impact: 5, Confidence: 0.9},
			},
		},
		"alpha": { // read mount, sorts first in fan-out order
			Total: 1, DefaultAxis: "impact",
			Highlights: []store.Highlight{
				{Path: "kb/dup.md", Title: "from read (alpha)", Type: "synthesis", Impact: 5, Confidence: 0.2},
			},
		},
	}
	s := &Server{Manager: m, providers: storeProviders{
		stats:    &lensStatsStub{byRepo: byRepo},
		activity: &lensActivityStub{byRepo: map[string]store.ActivityResult{}},
	}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)

	body := decodeLensStats(t, getLensFacts(t, r, "/lenses/eng/stats"))

	if len(body.Highlights) != 1 {
		t.Fatalf("highlights: got %d, want 1 after dedupe", len(body.Highlights))
	}
	if got := body.Highlights[0].Title; got != "from write (zulu)" {
		t.Errorf("dedupe winner: got title %q, want the write mount's copy (\"from write (zulu)\") even though it sorts after the read mount", got)
	}
}

// TestLensStats_OmittedAxisWithDisagreeingMountsUsesFullCandidatePool:
// store.Stats cuts each mount's SQL top-N by NormalizeAxis(requested axis,
// THAT MOUNT's own default). With axis omitted and mounts disagreeing on
// their own default, a naive single fan-out would leave the impact-default
// mount's candidates cut BY IMPACT while the union ranks by confidence (its
// resolved default) — silently dropping that mount's true top-N-by-
// confidence facts that fell outside its impact top-N. The union must
// re-fan-out at the resolved axis so every mount's candidate pool matches
// what it is ranked by.
func TestLensStats_OmittedAxisWithDisagreeingMountsUsesFullCandidatePool(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		// alpha (write, default impact): its impact-cut first pass carries
		// only a low-confidence fact — its true highest-confidence fact is
		// NOT in this list, mirroring a real impact-ranked SQL LIMIT.
		"alpha": {
			Total: 1, DefaultAxis: "impact",
			Highlights: []store.Highlight{
				{Path: "kb/a-impact-only.md", Title: "impact-only", Type: "synthesis", Impact: 9, Confidence: 0.1},
			},
		},
		// beta (read, default confidence): axis-insensitive in this stub —
		// same result whichever axis is requested.
		"beta": {
			Total: 1, DefaultAxis: "confidence",
			Highlights: []store.Highlight{
				{Path: "kb/b.md", Title: "beta", Type: "synthesis", Impact: 1, Confidence: 0.5},
			},
		},
	}
	byRepoAxis := map[string]map[string]store.StatsResult{
		// alpha's confidence-cut candidate pool — what a corrective re-fetch
		// at the resolved union axis (confidence) must return, INCLUDING the
		// high-confidence fact the impact-cut first pass omitted.
		"alpha": {
			"confidence": {
				Total: 1, DefaultAxis: "impact",
				Highlights: []store.Highlight{
					{Path: "kb/a-high-conf.md", Title: "high-conf", Type: "synthesis", Impact: 1, Confidence: 0.99},
					{Path: "kb/a-impact-only.md", Title: "impact-only", Type: "synthesis", Impact: 9, Confidence: 0.1},
				},
			},
		},
	}
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{
		stats:    &lensStatsStub{byRepo: byRepo, byRepoAxis: byRepoAxis},
		activity: &lensActivityStub{byRepo: map[string]store.ActivityResult{}},
	}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensStats(t, getLensFacts(t, r, "/lenses/eng/stats"))

	if body.DefaultAxis != "confidence" {
		t.Fatalf("default_axis: got %q, want confidence (mounts disagree)", body.DefaultAxis)
	}
	if len(body.Highlights) != 3 {
		t.Fatalf("highlights: got %d, want 3 (alpha's confidence-cut pool [2] + beta [1]); "+
			"a mismatch means alpha was never re-fetched at the resolved axis", len(body.Highlights))
	}
	if got := body.Highlights[0].Path; got != "kb/a-high-conf.md" {
		t.Errorf("top by confidence: got %q, want kb/a-high-conf.md (0.99) — "+
			"absent entirely means alpha's impact-cut first pass was used unchanged", got)
	}
}

// TestLensStats_HighlightsCapAtTen: three mounts of 10 each must yield 10.
//
// TopLayerFacts/TopLayerEdges are set purely so the pooled default_axis
// resolves to impact, matching this fixture's per-mount DefaultAxis — the cap
// behaviour under test is orthogonal to axis pooling.
func TestLensStats_HighlightsCapAtTen(t *testing.T) {
	mk := func(prefix string, base int) []store.Highlight {
		out := make([]store.Highlight, 0, 10)
		for i := 0; i < 10; i++ {
			out = append(out, store.Highlight{
				Path:  prefix + string(rune('a'+i)) + ".md",
				Title: "t", Type: "synthesis", Impact: base + i,
			})
		}
		return out
	}
	byRepo := map[string]store.StatsResult{
		"alpha": {Total: 10, DefaultAxis: "impact", TopLayerFacts: 1, TopLayerEdges: 5, Highlights: mk("kb/x/", 0)},
		"beta":  {Total: 10, DefaultAxis: "impact", TopLayerFacts: 1, TopLayerEdges: 5, Highlights: mk("kb/y/", 100)},
	}
	body := getLensStatsBody(t, byRepo)

	if len(body.Highlights) != 10 {
		t.Fatalf("highlights: got %d, want 10", len(body.Highlights))
	}
	if body.Highlights[0].Impact != 109 {
		t.Errorf("top impact: got %d, want 109", body.Highlights[0].Impact)
	}
}

// TestLensStats_MountWithNoEligibleFactsDoesNotFail: a mount holding only
// observations contributes nothing to highlights and must not shrink or fail
// the union (RFC §9.1 — a lens never silently shrinks its read set).
func TestLensStats_MountWithNoEligibleFactsDoesNotFail(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		"alpha": {
			Total: 1, DefaultAxis: "impact",
			Highlights: []store.Highlight{
				{Path: "kb/a.md", Title: "only one", Type: "synthesis", Impact: 4},
			},
		},
		"beta": {Total: 500, DefaultAxis: "impact", Highlights: []store.Highlight{}},
	}
	body := getLensStatsBody(t, byRepo)

	if len(body.Highlights) != 1 {
		t.Fatalf("highlights: got %d, want 1", len(body.Highlights))
	}
	if body.Total != 501 {
		t.Errorf("total: got %d, want 501 — the empty mount still counts", body.Total)
	}
}

// TestLensStats_DefaultAxisPoolsAboveThresholdEvenWhenOneMountDissents is the
// case the old AND-of-mounts rule got backwards (kb finding I1): alpha alone
// clears the 3.0 separation ratio (topMean=100/10=10, obsMean=10/10=1, ratio
// 10x -> its own default is impact) while beta alone does not (topMean=1/1=1,
// obsMean=1/1=1, ratio 1x -> confidence). Pooled: topMean=(100+1)/(10+1)=9.2,
// obsMean=(10+1)/(10+1)=1, ratio 9.2x -> impact. An AND rule would let beta's
// dissent veto the whole lens; pooling correctly lets the combined evidence
// decide.
func TestLensStats_DefaultAxisPoolsAboveThresholdEvenWhenOneMountDissents(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		"alpha": {
			Total: 20, DefaultAxis: "impact",
			TopLayerFacts: 10, TopLayerEdges: 100,
			ObservationFacts: 10, ObservationEdges: 10,
		},
		"beta": {
			Total: 2, DefaultAxis: "confidence",
			TopLayerFacts: 1, TopLayerEdges: 1,
			ObservationFacts: 1, ObservationEdges: 1,
		},
	}
	body := getLensStatsBody(t, byRepo)
	if body.DefaultAxis != "impact" {
		t.Errorf("default_axis: got %q, want impact — pooled ratio 9.2x clears 3.0 even though beta alone dissents", body.DefaultAxis)
	}
}

// TestLensStats_DefaultAxisConfidenceWhenPoolStaysBelowThreshold: both mounts
// individually AND pooled sit at a 2.0x ratio, below the 3.0 threshold — a
// genuine confidence case, not a pooling artifact.
func TestLensStats_DefaultAxisConfidenceWhenPoolStaysBelowThreshold(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		"alpha": {
			Total: 20, DefaultAxis: "confidence",
			TopLayerFacts: 10, TopLayerEdges: 20,
			ObservationFacts: 10, ObservationEdges: 10,
		},
		"beta": {
			Total: 20, DefaultAxis: "confidence",
			TopLayerFacts: 10, TopLayerEdges: 20,
			ObservationFacts: 10, ObservationEdges: 10,
		},
	}
	body := getLensStatsBody(t, byRepo)
	if body.DefaultAxis != "confidence" {
		t.Errorf("default_axis: got %q, want confidence — pooled ratio 2.0x stays below 3.0", body.DefaultAxis)
	}
}

// TestLensStats_DefaultAxisIgnoresMountWithZeroFacts: an empty mount (the
// "all" lens's zero-fact "test" repo, in the real corpus this bug was found
// against) must not veto or otherwise change the pooled outcome — it sums
// (0,0) into both sides of the ratio.
func TestLensStats_DefaultAxisIgnoresMountWithZeroFacts(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		"alpha": {
			Total: 20, DefaultAxis: "impact",
			TopLayerFacts: 10, TopLayerEdges: 100,
			ObservationFacts: 10, ObservationEdges: 10,
		},
		"beta": {Total: 0, DefaultAxis: "confidence"},
	}
	body := getLensStatsBody(t, byRepo)
	if body.DefaultAxis != "impact" {
		t.Errorf("default_axis: got %q, want impact — an empty mount (beta, zero facts) must not veto the pooled ratio", body.DefaultAxis)
	}
}

// TestLensStats_UnionDefaultAxisImpactWhenAllMountsAgree is the M6 gap: prior
// coverage only caught default_axis indirectly through highlight row order,
// whose failure message never names the axis. Assert it directly for the
// straightforward case where every mount agrees and the pool clears 3.0.
func TestLensStats_UnionDefaultAxisImpactWhenAllMountsAgree(t *testing.T) {
	byRepo := map[string]store.StatsResult{
		"alpha": {
			Total: 10, DefaultAxis: "impact",
			TopLayerFacts: 5, TopLayerEdges: 50,
			ObservationFacts: 5, ObservationEdges: 5,
		},
		"beta": {
			Total: 10, DefaultAxis: "impact",
			TopLayerFacts: 5, TopLayerEdges: 50,
			ObservationFacts: 5, ObservationEdges: 5,
		},
	}
	body := getLensStatsBody(t, byRepo)
	if body.DefaultAxis != "impact" {
		t.Errorf("default_axis: got %q, want impact when every mount agrees and the pool clears 3.0", body.DefaultAxis)
	}
}
