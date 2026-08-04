package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// lensStatsStub is a per-repo statsProvider: each mount's Stats call is
// answered from byRepo keyed by the repo's name, so a single stub drives the
// whole fan-out. It records the last pathPrefix seen per repo so tests can
// assert per-target path forwarding; errRepo makes that one mount fail.
type lensStatsStub struct {
	byRepo   map[string]store.StatsResult
	errRepo  string
	lastPath map[string]string
}

func (s *lensStatsStub) Stats(_ context.Context, ri *repos.RepoInstance, _ string, pathPrefix, _ string) (store.StatsResult, error) {
	if s.lastPath == nil {
		s.lastPath = map[string]string{}
	}
	s.lastPath[ri.Name()] = pathPrefix
	if s.errRepo != "" && ri.Name() == s.errRepo {
		return store.StatsResult{}, errors.New("db on fire")
	}
	return s.byRepo[ri.Name()], nil
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
	Repos         []struct {
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
		"alpha": {Total: 200, AvgConfidence: 0.9, Domains: map[string]int{"go": 5, "ai": 10}, Entities: map[string]int{"chi": 3}},
		"beta":  {Total: 50, AvgConfidence: 0.5, Domains: map[string]int{"go": 2, "web": 1}, Entities: map[string]int{"vite": 4}},
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
	if body.Domains == nil || body.Entities == nil {
		t.Error("union domains/entities must be {} not null")
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
