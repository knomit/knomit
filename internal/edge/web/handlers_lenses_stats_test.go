package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// The lens union stats tests run over REAL stores: the mounts are preset repos
// (newTestLensManager), each test writes the facts it needs into them, and a
// failing or slow mount is made so through the owner's per-Service test hooks
// (index.Facade.SetReadFaultForTest for a failure, SetOverridesForTest's
// observer for a delay or a call count) rather than through a stub of the
// handler's data source.

// newLensStatsRouter builds started preset repos with the given names and a
// lens "eng" whose write repo is names[0] and whose read mounts are the rest.
func newLensStatsRouter(t *testing.T, names ...string) (*repos.Manager, http.Handler) {
	t.Helper()
	m, _ := newTestLensManager(t, names...)
	r := (&Server{Manager: m}).NewAPIRouter()
	reads := make([]string, 0, len(names)-1)
	for _, n := range names[1:] {
		reads = append(reads, fmt.Sprintf(`{"repo":%q}`, n))
	}
	createLens(t, m, r, fmt.Sprintf(`{"name":"eng","write":%q,"reads":[%s]}`, names[0], strings.Join(reads, ",")))
	return m, r
}

// obs writes an observation to ri; syn writes a synthesis deriving from refs
// (its impact is the number of refs); both carry no domain or entity.
func obs(t *testing.T, ri *repos.RepoInstance, path, title string, conf float64, refs ...string) {
	t.Helper()
	writeTypedFact(t, ri, path, title, fact.Observation, conf, refs, nil, nil)
}

func syn(t *testing.T, ri *repos.RepoInstance, path, title string, conf float64, refs ...string) {
	t.Helper()
	writeTypedFact(t, ri, path, title, fact.Synthesis, conf, refs, nil, nil)
}

// seedAxisMount writes the corpus whose OWN default axis is impact: one
// observation with no out-edges and one synthesis deriving from it.
func seedAxisMount(t *testing.T, ri *repos.RepoInstance, tag string, impact int) {
	t.Helper()
	refs := make([]string, 0, impact)
	for i := 0; i < impact; i++ {
		p := fmt.Sprintf("kb/o/%s%d.md", tag, i)
		obs(t, ri, p, "obs "+p, 0.5)
		refs = append(refs, p)
	}
	syn(t, ri, "kb/s/"+tag+".md", "syn "+tag, 0.8, refs...)
}

// withMountStore runs fn with the live store of the named mount.
func withMountStore(t *testing.T, m *repos.Manager, name string, fn func(svc *store.Service)) {
	t.Helper()
	require.NoError(t, m.Get(name).WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "mount %s must have a live store", name)
		fn(svc)
	}))
}

// failMountRead fails the named read on this mount's store only, with err, and
// counts how often it was hit (atomic: the fan-out reads every mount on its own
// goroutine).
func failMountRead(t *testing.T, m *repos.Manager, name string, read index.ReadName, err error, hits *atomic.Int64) {
	t.Helper()
	withMountStore(t, m, name, func(svc *store.Service) {
		t.Cleanup(svc.Search().SetReadFaultForTest(failIndexRead(read, err, hits)))
	})
}

// slowMountRead makes the named public read of this mount's store take d. It
// goes through the index.Overrides observer, which is per Service, so each
// mount can be given its own latency.
func slowMountRead(t *testing.T, m *repos.Manager, name, method string, d time.Duration) {
	t.Helper()
	withMountStore(t, m, name, func(svc *store.Service) {
		t.Cleanup(svc.Search().SetOverridesForTest(index.Overrides{Reads: func(got string) error {
			if got == method {
				time.Sleep(d)
			}
			return nil
		}}))
	})
}

// countMountReads records, per method, how often each mount's store was read.
// The map is fixed before use and holds atomic counters, so the closure the
// fan-out goroutines call is race-free.
type readCounts map[string]*atomic.Int64

func countMountReads(t *testing.T, m *repos.Manager, names []string, methods ...string) map[string]readCounts {
	t.Helper()
	out := map[string]readCounts{}
	for _, n := range names {
		rc := readCounts{}
		for _, meth := range methods {
			rc[meth] = &atomic.Int64{}
		}
		out[n] = rc
		withMountStore(t, m, n, func(svc *store.Service) {
			t.Cleanup(svc.Search().SetOverridesForTest(index.Overrides{Reads: func(got string) error {
				if c, ok := rc[got]; ok {
					c.Add(1)
				}
				return nil
			}}))
		})
	}
	return out
}

// repoWire GETs a repo-scoped endpoint of one lens mount.
func repoWire(t *testing.T, r http.Handler, name, endpoint string) []byte {
	t.Helper()
	rec := getLensFacts(t, r, "/repos/"+name+"/branches/machine:test/"+endpoint)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.Bytes()
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

func (b lensStatsBody) highlightTitles() []string {
	out := make([]string, 0, len(b.Highlights))
	for _, h := range b.Highlights {
		out = append(out, h.Title)
	}
	return out
}

const lensStatsURL = "/lenses/eng/stats"

// The union roll-up sums totals/domains/entities/types across mounts, weights
// avg_confidence by per-mount total, and lists one per-repo row per fanned-out
// target - is_write set on the write repo only, id = the mount's id12, branch
// = the Binding-resolved read branch - each carrying that mount's own stats and
// its own commit activity (the same numbers the repo endpoints answer).
func TestLensStats_UnionAggregates(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	writeTypedFact(t, alpha, "kb/technology/a1.md", "a1", fact.Observation, 0.9, nil, []string{"go", "ai"}, []string{"chi"})
	writeTypedFact(t, alpha, "kb/technology/a2.md", "a2", fact.Synthesis, 0.9, []string{"kb/technology/a1.md"}, []string{"ai"}, nil)
	writeTypedFact(t, beta, "kb/technology/b1.md", "b1", fact.Observation, 0.6, nil, []string{"go", "web"}, []string{"vite"})

	rec := getLensFacts(t, r, lensStatsURL)
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	body := decodeLensStats(t, rec)

	require.Equal(t, 3, body.Total, "sum across mounts")
	require.Equal(t, 2, body.RepoCount)
	// Total-weighted mean: (0.9*2 + 0.6*1) / 3 = 0.8.
	require.InDelta(t, 0.8, body.AvgConfidence, 1e-9, "total-weighted mean")
	require.Equal(t, map[string]int{"go": 2, "ai": 2, "web": 1}, body.Domains)
	require.Equal(t, map[string]int{"chi": 1, "vite": 1}, body.Entities)
	require.Equal(t, map[string]int{"observation": 2, "synthesis": 1}, body.Types)

	require.Len(t, body.Repos, 2)
	// Reads() is sorted by repo name: alpha (write) then beta.
	a, b := body.Repos[0], body.Repos[1]
	require.Equal(t, "alpha", a.Name)
	require.True(t, a.IsWrite)
	require.Equal(t, "beta", b.Name)
	require.False(t, b.IsWrite)
	require.Equal(t, federate.ID12(alpha.ID()), a.ID)
	require.Equal(t, federate.ID12(beta.ID()), b.ID)
	require.Equal(t, alpha.AgentBranch(), a.Branch)
	require.Equal(t, beta.AgentBranch(), b.Branch)
	require.Equal(t, 2, a.Total)
	require.Equal(t, 0.9, a.AvgConfidence)
	require.Equal(t, map[string]int{"go": 1, "ai": 2}, a.Domains)
	require.Equal(t, 1, b.Total)
	require.Equal(t, 0.6, b.AvgConfidence)
	require.Equal(t, map[string]int{"vite": 1}, b.Entities)

	// Each row carries ITS mount's activity: exactly what the repo's own
	// /activity answers, with the two facts written to alpha and the one written
	// to beta on top of the commits each preset repo already had.
	for _, row := range body.Repos {
		own := decodeActivity(t, repoWire(t, r, row.Name, "activity"))
		require.Equal(t, own.LastCommit, row.LastCommit, row.Name)
		require.Equal(t, own.Changes7d, row.Changes7d, row.Name)
		require.Equal(t, own.Changes30d, row.Changes30d, row.Name)
		require.Equal(t, own.Changes90d, row.Changes90d, row.Name)
	}
	require.Greater(t, body.Repos[0].Changes7d, body.Repos[1].Changes7d, "alpha holds one more commit than beta")
}

// last_commit is the MAX stamp across mounts, not the first, the last or the
// min. beta - in the MIDDLE of binding order - commits a second after alpha and
// gamma, whose newest commit is their preset's.
func TestLensStats_LastCommitIsTheMaxAcrossMounts(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta", "gamma")
	// Commit stamps have one-second resolution.
	time.Sleep(1100 * time.Millisecond)
	obs(t, m.Get("beta"), "kb/o/late.md", "late", 0.5)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Len(t, body.Repos, 3)
	alpha, beta, gamma := body.Repos[0], body.Repos[1], body.Repos[2]
	require.Equal(t, "beta", beta.Name)
	require.Greater(t, beta.LastCommit, alpha.LastCommit)
	require.Greater(t, beta.LastCommit, gamma.LastCommit)
	require.Equal(t, beta.LastCommit, body.LastCommit)
}

// repo=<name> narrows the fan-out to the named mount(s): the roll-up and
// repo_count reflect only those mounts.
func TestLensStats_RepoFilterNarrows(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	obs(t, m.Get("alpha"), "kb/o/a1.md", "a1", 0.9)
	obs(t, m.Get("alpha"), "kb/o/a2.md", "a2", 0.9)
	obs(t, m.Get("beta"), "kb/o/b1.md", "b1", 0.5)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL+"?repo=beta"))

	require.Equal(t, 1, body.RepoCount, "narrowed")
	require.Equal(t, 1, body.Total, "beta only")
	require.Len(t, body.Repos, 1)
	require.Equal(t, "beta", body.Repos[0].Name)
}

// An unknown repo= name is a well-formed request naming a nonexistent mount -> 422.
func TestLensStats_UnknownRepoFilter422(t *testing.T) {
	_, r := newLensStatsRouter(t, "alpha", "beta")

	rec := getLensFacts(t, r, lensStatsURL+"?repo=ghost")

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
}

// A lens over mounts holding no facts yields zero totals, avg_confidence 0 (no
// division by zero), NON-NULL maps at both the union and per-repo levels, and a
// per-repo row per mount. The types union serializes as the literal `{}`, not
// `null`: the typed assertions only prove a nil map decodes as non-nil, so the
// exact-string check is what pins the wire representation.
func TestLensStats_EmptyMounts(t *testing.T) {
	_, r := newLensStatsRouter(t, "alpha", "beta")

	rec := getLensFacts(t, r, lensStatsURL)
	body := decodeLensStats(t, rec)

	require.Zero(t, body.Total)
	require.Zero(t, body.AvgConfidence)
	require.NotNil(t, body.Domains)
	require.NotNil(t, body.Entities)
	require.NotNil(t, body.Types)
	require.Len(t, body.Repos, 2, "mounts still listed")
	for _, row := range body.Repos {
		require.NotNil(t, row.Domains, row.Name)
		require.NotNil(t, row.Entities, row.Name)
	}
	require.Contains(t, rec.Body.String(), `"types":{}`)
	require.Contains(t, rec.Body.String(), `"highlights":[]`)
}

// Any mount error fails the WHOLE request (RFC 9.1) - even when the failing
// mount is a read mount and the write repo answered fine - for BOTH the stats
// and the activity read. The 500 carries THAT mount's injected error, and the
// fault fired for the named read on that one mount exactly once.
func TestLensStats_MountErrorFailsWholeRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		read index.ReadName
	}{
		{"stats error", index.ReadStats},
		{"activity error", index.ReadActivity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r := newLensStatsRouter(t, "alpha", "beta")
			injected := errors.New("injected beta " + string(tc.read) + " failure")
			var hits atomic.Int64
			failMountRead(t, m, "beta", tc.read, injected, &hits)

			rec := getLensFacts(t, r, lensStatsURL)

			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
			require.Contains(t, rec.Body.String(), injected.Error())
			require.EqualValues(t, 1, hits.Load(), "beta's %s read must have reached the hook exactly once", tc.read)
		})
	}
}

// path= is applied on EVERY mount, for both the stats and the activity read, as
// the per-target (repo-relative) prefix: only the facts - and the commits -
// under it count.
func TestLensStats_ForwardsPathPrefix(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	obs(t, m.Get("alpha"), "kb/meta/a1.md", "a1", 0.9)
	obs(t, m.Get("alpha"), "kb/other/a2.md", "a2", 0.9)
	obs(t, m.Get("beta"), "kb/meta/b1.md", "b1", 0.9)
	obs(t, m.Get("beta"), "kb/other/b2.md", "b2", 0.9)
	obs(t, m.Get("beta"), "kb/other/b3.md", "b3", 0.9)

	whole := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))
	require.Equal(t, 5, whole.Total)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL+"?path=kb/meta"))

	require.Equal(t, 2, body.Total, "one fact per mount under kb/meta")
	require.Len(t, body.Repos, 2)
	for _, row := range body.Repos {
		require.Equal(t, 1, row.Total, "stats path for %s", row.Name)
		require.Equal(t, 1, row.Changes7d, "activity path for %s", row.Name)
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensStats_UnknownLens404(t *testing.T) {
	_, r := newLensStatsRouter(t, "alpha")

	rec := getLensFacts(t, r, "/lenses/missing/stats")

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// mount B holds the highest-impact fact; it must lead even though mount A is
// returned first. Asserted by TITLE, not path: beta is a read mount, so its path
// is qualified on the wire.
func TestLensStats_HighlightsAreGlobalTopNotFirstMount(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedAxisMount(t, m.Get("alpha"), "a", 1)
	seedAxisMount(t, m.Get("beta"), "b", 3)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, "impact", body.DefaultAxis)
	require.Equal(t, []string{"syn b", "syn a"}, body.highlightTitles(),
		"the impact-3 highlight from the read mount leads")
}

// Seeds the same fact path on a write mount and a read mount (a re-rooted fork
// shares fact UUIDs, so the same path can arrive twice). The two copies carry
// DIFFERENT title and confidence so a test can tell WHICH copy survived.
func seedDuplicatePath(t *testing.T, write, read *repos.RepoInstance) {
	t.Helper()
	for _, ri := range []*repos.RepoInstance{write, read} {
		obs(t, ri, "kb/o/src.md", "source", 0.5)
	}
	syn(t, write, "kb/dup.md", "from write ("+write.Name()+")", 0.9, "kb/o/src.md")
	syn(t, read, "kb/dup.md", "from read ("+read.Name()+")", 0.2, "kb/o/src.md")
}

// Unlike the aggregate sums, highlights carry paths and MUST dedupe - write
// mount wins, matching federate.WriteFirstWinners (the facts/search/topics
// unions' shared dedup).
func TestLensStats_HighlightsDedupeAcrossMounts(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedDuplicatePath(t, m.Get("alpha"), m.Get("beta"))

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Len(t, body.Highlights, 1, "one highlight after dedupe")
	require.Equal(t, "from write (alpha)", body.Highlights[0].Title, "the write mount's copy wins")
	require.Equal(t, 0.9, body.Highlights[0].Confidence)
}

// control.Lens.normalize sorts Reads alphabetically by repo name, so the write
// mount is NOT necessarily first in fan-out order - a "first occurrence in
// iteration order wins" dedup would pick the read mount's copy here. Write must
// win regardless of where it sorts.
func TestLensStats_HighlightsDedupeWriteWinsEvenWhenWriteSortsAfterRead(t *testing.T) {
	m, r := newLensStatsRouter(t, "zulu", "alpha")
	seedDuplicatePath(t, m.Get("zulu"), m.Get("alpha"))

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Len(t, body.Highlights, 1)
	require.Equal(t, "from write (zulu)", body.Highlights[0].Title,
		"the write mount's copy wins even though it sorts after the read mount")
}

// With axis omitted and mounts disagreeing on their own default, a naive single
// fan-out would leave the impact-default mount's candidates cut BY IMPACT while
// the union ranks by confidence (its resolved default) - silently dropping that
// mount's true top-N-by-confidence facts that fell outside its impact top-N.
// The union must re-fan-out at the resolved axis.
//
// alpha holds 11 syntheses: ten with impact 1 and low confidence, and one with
// impact 0 and confidence 0.99 - which a top-10 cut by impact leaves out. alpha
// alone resolves to impact (its observation has no out-edges). beta alone
// resolves to confidence, and its observations' out-edges drag the POOLED ratio
// under the threshold, so the union resolves to confidence.
func TestLensStats_OmittedAxisWithDisagreeingMountsUsesFullCandidatePool(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	obs(t, alpha, "kb/o/a.md", "alpha source", 0.5)
	for i := 0; i < 10; i++ {
		syn(t, alpha, fmt.Sprintf("kb/s/low%d.md", i), fmt.Sprintf("low %d", i), 0.1, "kb/o/a.md")
	}
	syn(t, alpha, "kb/s/high.md", "high-conf", 0.99)
	// beta: observation b_j derives from every earlier one (mean out-degree 3),
	// and its one synthesis derives from nothing.
	var earlier []string
	for j := 0; j < 7; j++ {
		p := fmt.Sprintf("kb/o/b%d.md", j)
		obs(t, beta, p, p, 0.5, earlier...)
		earlier = append(earlier, p)
	}
	syn(t, beta, "kb/s/beta.md", "beta syn", 0.5)
	require.Equal(t, "impact", decodeStats(t, repoWire(t, r, "alpha", "stats")).DefaultAxis)
	require.Equal(t, "confidence", decodeStats(t, repoWire(t, r, "beta", "stats")).DefaultAxis)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, "confidence", body.DefaultAxis, "mounts disagree and the pooled evidence points at confidence")
	require.NotEmpty(t, body.Highlights)
	require.Equal(t, "high-conf", body.Highlights[0].Title,
		"absent means alpha's impact-cut first pass was used unchanged")
	require.Len(t, body.Highlights, index.MaxHighlights)
}

// Two mounts of ten highlights each must yield MaxHighlights, and the cap
// keeps the best-ranked ones.
func TestLensStats_HighlightsCapAtTen(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	obs(t, alpha, "kb/o/a.md", "a src", 0.5)
	obs(t, beta, "kb/o/b0.md", "b src 0", 0.5)
	obs(t, beta, "kb/o/b1.md", "b src 1", 0.5)
	obs(t, beta, "kb/o/b2.md", "b src 2", 0.5)
	for i := 0; i < 10; i++ {
		syn(t, alpha, fmt.Sprintf("kb/s/a%d.md", i), fmt.Sprintf("alpha %d", i), 0.5, "kb/o/a.md")
		syn(t, beta, fmt.Sprintf("kb/s/b%d.md", i), fmt.Sprintf("beta %d", i), 0.5, "kb/o/b0.md", "kb/o/b1.md", "kb/o/b2.md")
	}

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Len(t, body.Highlights, index.MaxHighlights)
	for _, h := range body.Highlights {
		require.Equal(t, 3, h.Impact, "the cap keeps the top-ranked, all from beta: %s", h.Title)
	}
}

// A mount holding only observations contributes nothing to highlights and must
// not shrink or fail the union (RFC 9.1 - a lens never silently shrinks its
// read set): its facts still count.
func TestLensStats_MountWithNoEligibleFactsDoesNotFail(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedAxisMount(t, m.Get("alpha"), "a", 1)
	for i := 0; i < 5; i++ {
		obs(t, m.Get("beta"), fmt.Sprintf("kb/o/b%d.md", i), "b", 0.5)
	}

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, []string{"syn a"}, body.highlightTitles())
	require.Equal(t, 7, body.Total, "the observation-only mount still counts")
}

// requireOwnAxes asserts, from the repo endpoints themselves, what each mount
// would recommend on its own - so a test about POOLING can prove the mounts
// disagree (or agree) before the union is read.
func requireOwnAxes(t *testing.T, r http.Handler, want map[string]string) {
	t.Helper()
	for name, axis := range want {
		require.Equal(t, axis, decodeStats(t, repoWire(t, r, name, "stats")).DefaultAxis, "own axis of %s", name)
	}
}

// The case the old AND-of-mounts rule got backwards: alpha alone clears the 3.0
// separation ratio (top mean 4, observation mean 0: impact) while beta alone
// does not (top mean 1, observation mean 0.5: confidence). Pooled: 3 top facts
// with 9 edges against 6 observations with 1 edge - 18x - so impact. An AND
// rule would let beta's dissent veto the whole lens; pooling lets the combined
// evidence decide.
func TestLensStats_DefaultAxisPoolsAboveThresholdEvenWhenOneMountDissents(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	var four []string
	for i := 0; i < 4; i++ {
		p := fmt.Sprintf("kb/o/a%d.md", i)
		obs(t, alpha, p, p, 0.5)
		four = append(four, p)
	}
	syn(t, alpha, "kb/s/a0.md", "a0", 0.8, four...)
	syn(t, alpha, "kb/s/a1.md", "a1", 0.8, four...)
	obs(t, beta, "kb/o/q1.md", "q1", 0.5)
	obs(t, beta, "kb/o/q2.md", "q2", 0.5, "kb/o/q1.md")
	syn(t, beta, "kb/s/q.md", "q", 0.8, "kb/o/q1.md")
	requireOwnAxes(t, r, map[string]string{"alpha": "impact", "beta": "confidence"})

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, "impact", body.DefaultAxis,
		"the pooled ratio clears 3.0 even though beta alone dissents")
}

// Both mounts individually AND pooled sit at a 2.0x ratio, below the 3.0
// threshold - a genuine confidence case, not a pooling artifact. Each mount:
// one synthesis deriving from two of three observations whose own out-edges
// total three.
func TestLensStats_DefaultAxisConfidenceWhenPoolStaysBelowThreshold(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	for _, name := range []string{"alpha", "beta"} {
		ri := m.Get(name)
		obs(t, ri, "kb/o/1.md", "o1", 0.5)
		obs(t, ri, "kb/o/2.md", "o2", 0.5, "kb/o/1.md")
		obs(t, ri, "kb/o/3.md", "o3", 0.5, "kb/o/1.md", "kb/o/2.md")
		syn(t, ri, "kb/s/s.md", "s", 0.8, "kb/o/1.md", "kb/o/2.md")
	}
	requireOwnAxes(t, r, map[string]string{"alpha": "confidence", "beta": "confidence"})

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, "confidence", body.DefaultAxis, "pooled ratio 2.0x stays below 3.0")
}

// An empty mount (the "all" lens's zero-fact "test" repo, in the real corpus
// this bug was found against) must not veto or otherwise change the pooled
// outcome - it sums (0,0) into both sides of the ratio.
func TestLensStats_DefaultAxisIgnoresMountWithZeroFacts(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedAxisMount(t, m.Get("alpha"), "a", 2)
	requireOwnAxes(t, r, map[string]string{"alpha": "impact", "beta": "confidence"})

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, "impact", body.DefaultAxis, "an empty mount must not veto the pooled ratio")
}

// default_axis asserted directly (rather than through highlight row order,
// whose failure message never names the axis) for the straightforward case
// where every mount agrees and the pool clears 3.0.
func TestLensStats_UnionDefaultAxisImpactWhenAllMountsAgree(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedAxisMount(t, m.Get("alpha"), "a", 2)
	seedAxisMount(t, m.Get("beta"), "b", 2)
	requireOwnAxes(t, r, map[string]string{"alpha": "impact", "beta": "impact"})

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, "impact", body.DefaultAxis)
}

// Clicking a highlight on the summary dashboard sent the mount-RELATIVE path to
// /lenses/{lens}/facts/*, which resolves a bare path against the WRITE repo, so
// every highlight owned by a read mount 404'd. /facts and /search return
// kb://<id12>/... for read-mount rows; /stats must too, while the write
// mount's paths stay bare.
func TestLensStats_HighlightsCarryQualifiedPaths(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedAxisMount(t, m.Get("alpha"), "w", 1)
	seedAxisMount(t, m.Get("beta"), "r", 1)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	byTitle := map[string]string{}
	for _, h := range body.Highlights {
		byTitle[h.Title] = h.Path
	}
	require.Equal(t, "kb/s/w.md", byTitle["syn w"], "write mount highlight keeps the bare path")
	require.Equal(t, federate.QualifyPath(federate.ID12(m.Get("beta").ID()), "kb/s/r.md"), byTitle["syn r"],
		"read mount highlight is qualified")
}

// Qualification must happen AFTER the dedup, never before: keying on already
// qualified paths would give the two copies different keys, so both would
// survive and the write mount would stop winning.
func TestLensStats_QualifyingDoesNotDefeatTheDedupe(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedDuplicatePath(t, m.Get("alpha"), m.Get("beta"))

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Len(t, body.Highlights, 1, "one highlight after dedupe")
	require.Equal(t, "from write (alpha)", body.Highlights[0].Title)
	require.Equal(t, "kb/dup.md", body.Highlights[0].Path, "the write mount's BARE path")
}

// -- The exclusion is about a SCOPE, and a union is a bigger one --------------
//
// store lets a scope holding nothing but observations have them as its
// highlights: the exclusion exists to stop observations burying the syntheses,
// and where there is no distilled layer to bury it only blanks the panel. A lens
// over a distilled mount plus a pure-observation mount HAS a distilled layer, so
// the fallback must not travel into the merge.

func TestLensStats_FallbackHighlightsStayOutOfAUnionWithRealOnes(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	// The distilled mount: a real highlight, modest confidence.
	writeTypedFact(t, m.Get("alpha"), "kb/s/1.md", "synthesis", fact.Synthesis, 0.7, nil, nil, nil)
	// Pure observation: the store had nothing eligible and answered with the
	// excluded types, flagging that it did.
	obs(t, m.Get("beta"), "kb/o/1.md", "observation", 0.99)

	// Explicit axis: confidence orders on confidence alone, so the observation
	// would lead the list if it got in.
	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL+"?axis=confidence"))

	require.Equal(t, []string{"synthesis"}, body.highlightTitles(),
		"a mount's fallback must not bury the union's distilled layer")
}

// The other half of the rule: when NO mount has a distilled layer, the union is
// itself the pure-observation scope, and blanking the panel is exactly the
// failure the store-side fallback was written to fix.
func TestLensStats_AllMountsPureObservationKeepTheirHighlights(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	obs(t, m.Get("alpha"), "kb/o/a.md", "obs-a", 0.5)
	obs(t, m.Get("beta"), "kb/o/b.md", "obs-b", 0.4)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL+"?axis=confidence"))

	require.Equal(t, []string{"obs-a", "obs-b"}, body.highlightTitles(), "both mounts' observations")
}

// A mount with no facts at all reports neither highlights nor a fallback, so it
// must not be mistaken for "the union has a distilled layer" - that would
// suppress the only highlights on offer and blank the panel.
func TestLensStats_AnEmptyMountDoesNotCountAsARealHighlightList(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	obs(t, m.Get("beta"), "kb/o/b.md", "obs-b", 0.5)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL+"?axis=confidence"))

	require.Equal(t, []string{"obs-b"}, body.highlightTitles(), "the read mount's own")
}

// The axis correction re-fetches HIGHLIGHTS, not the whole StatsResult.
// Everything else the union reads from a mount - total, avg_confidence, both
// histograms, type counts, separation counters - is axis-independent and already
// in hand after the first pass. Two counters, because "it got faster" is not
// something a unit test can see, but "it stopped asking for the aggregates
// twice" is.
//
// alpha defaults to impact, beta to confidence, and the pooled axis is impact:
// beta disagrees with the pooled verdict, which arms the corrective pass.
func TestLensStats_AxisCorrectionRefetchesOnlyHighlights(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedAxisMount(t, m.Get("alpha"), "a", 1)
	syn(t, m.Get("beta"), "kb/s/b.md", "syn b", 0.5)
	requireOwnAxes(t, r, map[string]string{"alpha": "impact", "beta": "confidence"})
	counts := countMountReads(t, m, []string{"alpha", "beta"}, "Stats", "Highlights")

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	require.Equal(t, "impact", body.DefaultAxis)
	for _, name := range []string{"alpha", "beta"} {
		require.EqualValues(t, 1, counts[name]["Highlights"].Load(),
			"%s: the corrective pass did not run, so this test is no longer measuring what it claims", name)
		require.EqualValues(t, 1, counts[name]["Stats"].Load(),
			"%s: a second full fan-out is the regression this test exists to catch", name)
	}
}

// With every mount already agreeing with the pooled verdict there is nothing to
// correct, and the narrow call must not fire at all - the fast path stays one
// fan-out.
func TestLensStats_AgreeingMountsSkipTheHighlightsRefetch(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	seedAxisMount(t, m.Get("alpha"), "a", 1)
	seedAxisMount(t, m.Get("beta"), "b", 1)
	requireOwnAxes(t, r, map[string]string{"alpha": "impact", "beta": "impact"})
	counts := countMountReads(t, m, []string{"alpha", "beta"}, "Stats", "Highlights")

	decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	for _, name := range []string{"alpha", "beta"} {
		require.EqualValues(t, 1, counts[name]["Stats"].Load(), name)
		require.Zero(t, counts[name]["Highlights"].Load(),
			"%s: nothing disagreed with the pooled axis", name)
	}
}

// -- concurrent fan-out: 9.1 and determinism ---------------------------------

// The response must be a function of the REQUEST, not of which mount finished
// first. The slowest mount is deliberately the FIRST in binding order, so a
// handler that assembled results as they arrived - appending from inside a
// goroutine - would emit the repos rows inverted, and the qualified paths and
// per-mount attribution with them.
func TestLensStats_ResponseOrderIsBindingOrderNotCompletionOrder(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta", "gamma")
	seedAxisMount(t, m.Get("alpha"), "a", 1)
	seedAxisMount(t, m.Get("beta"), "b", 3)
	obs(t, m.Get("gamma"), "kb/o/g.md", "g src", 0.5)
	syn(t, m.Get("gamma"), "kb/s/g.md", "syn g", 0.5)
	// Completion order is forced to the exact reverse of binding order.
	slowMountRead(t, m, "alpha", "Stats", 60*time.Millisecond)
	slowMountRead(t, m, "beta", "Stats", 30*time.Millisecond)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL))

	got := make([]string, 0, len(body.Repos))
	for _, row := range body.Repos {
		got = append(got, row.Name)
	}
	require.Equal(t, []string{"alpha", "beta", "gamma"}, got,
		"binding order; the slowest mount is first, so this is completion order leaking into the response")
	// Highlights are ranked by the axis, never by arrival: beta (impact 3) tops
	// the list despite finishing second. Read-mount paths are qualified, so
	// compare on title.
	require.Equal(t, []string{"syn b", "syn a", "syn g"}, body.highlightTitles())
	// The sums are order-independent, but assert them anyway: a reduction that
	// dropped a slow mount's contribution would still order correctly.
	require.Equal(t, 2+4+2, body.Total, "a mount's result was lost")
}

// 9.1 survives concurrency: one failing mount still fails the whole request,
// wherever it sits in the fan-out and however fast it fails relative to the
// others. The failing mount here finishes FIRST while two healthy mounts are
// still running, so an implementation that returned early on the first error
// would race the goroutines still writing their slots.
func TestLensStats_MountErrorFailsWholeRequestUnderConcurrency(t *testing.T) {
	for _, failing := range []string{"alpha", "beta", "gamma"} {
		t.Run("failing="+failing, func(t *testing.T) {
			m, r := newLensStatsRouter(t, "alpha", "beta", "gamma")
			injected := errors.New("injected stats failure of " + failing)
			var hits atomic.Int64
			failMountRead(t, m, failing, index.ReadStats, injected, &hits)
			// The failing mount answers immediately; the others are still in flight.
			for _, name := range []string{"alpha", "beta", "gamma"} {
				if name != failing {
					slowMountRead(t, m, name, "Stats", 40*time.Millisecond)
				}
			}

			rec := getLensFacts(t, r, lensStatsURL)

			require.Equal(t, http.StatusInternalServerError, rec.Code,
				"a lens must never silently shrink its read set (RFC 9.1); body=%s", rec.Body.String())
			require.Contains(t, rec.Body.String(), injected.Error())
			require.EqualValues(t, 1, hits.Load())
		})
	}
}

// When SEVERAL mounts fail, the reported error is deterministic: the
// LOWEST-INDEXED failing mount, which is the one a serial loop would have hit
// first. Otherwise the message a caller sees - and a lens error names the
// offending mount - would be decided by goroutine scheduling.
//
// gamma is made to fail FAST and beta SLOWLY, so "first to fail" and "first in
// binding order" disagree; the assertion is that binding order wins.
func TestLensStats_ConcurrentFailuresReportTheLowestIndexedMount(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta", "gamma")
	var betaHits, gammaHits atomic.Int64
	failMountRead(t, m, "beta", index.ReadStats, errors.New("db on fire: beta"), &betaHits)
	failMountRead(t, m, "gamma", index.ReadStats, errors.New("db on fire: gamma"), &gammaHits)
	slowMountRead(t, m, "beta", "Stats", 50*time.Millisecond)

	// Repeated, because a scheduling-dependent answer is exactly the failure
	// mode and one run could get lucky.
	const runs = 10
	for i := 0; i < runs; i++ {
		rec := getLensFacts(t, r, lensStatsURL)
		require.Equal(t, http.StatusInternalServerError, rec.Code, "run %d", i)
		require.Contains(t, rec.Body.String(), "db on fire: beta",
			"run %d: want the error of beta (mount 1), the lowest-indexed failing mount - "+
				"gamma fails sooner, so this is completion order deciding which error the caller sees", i)
		require.NotContains(t, rec.Body.String(), "db on fire: gamma", "run %d", i)
	}
	require.EqualValues(t, runs, betaHits.Load(), "beta's Stats read must run once per request")
	require.EqualValues(t, runs, gammaHits.Load(), "gamma's Stats read must run once per request")
}

// With path=, a mount that has no commit touching the path answers
// last_commit "" (the production subtree view of a lens dashboard), and "" must
// lose to any real stamp in the union. The only fact under the path is on the
// FIRST mount, so a last-wins reduction would return "".
func TestLensStats_LastCommitUnderAPathIgnoresMountsWithNoCommitThere(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta", "gamma")
	obs(t, m.Get("alpha"), "kb/only/a.md", "only", 0.5)

	body := decodeLensStats(t, getLensFacts(t, r, lensStatsURL+"?path=kb/only"))

	require.Len(t, body.Repos, 3)
	alpha, beta, gamma := body.Repos[0], body.Repos[1], body.Repos[2]
	require.Equal(t, "alpha", alpha.Name)
	require.NotEmpty(t, alpha.LastCommit)
	require.Empty(t, beta.LastCommit, "beta has no commit under the path")
	require.Empty(t, gamma.LastCommit, "gamma has no commit under the path")
	require.Equal(t, alpha.LastCommit, body.LastCommit, "the union is the first mount's stamp")

	// Narrowed to a mount with nothing under the path, the union is "".
	empty := decodeLensStats(t, getLensFacts(t, r, lensStatsURL+"?repo=beta&path=kb/only"))
	require.Len(t, empty.Repos, 1)
	require.Empty(t, empty.LastCommit)
}
