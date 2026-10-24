package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// writeTypedFact writes one fact of the given epistemic type to the agent
// branch of ri's real store. refs are local fact paths and become DERIVED_FROM
// edges (the fact's impact), so they must already exist.
func writeTypedFact(t *testing.T, ri *repos.RepoInstance, path, title string, typ fact.Type, conf float64, refs, domains, entities []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = "body"
	f.Kind = fact.Epistemic
	f.Type = typ
	f.Confidence = conf
	f.Sources = 1
	f.Domain = domains
	f.Entities = entities
	f.Refs = refs
	text, err := fact.SerializeFact(f)
	require.NoError(t, err)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "the test repo must have a live store")
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path, text, "add "+path, "")
		require.NoError(t, err)
	}))
}

// statsWire mirrors the repo /stats wire shape.
type statsWire struct {
	Total         int            `json:"total"`
	AvgConfidence float64        `json:"avg_confidence"`
	Domains       map[string]int `json:"domains"`
	Entities      map[string]int `json:"entities"`
	Types         map[string]int `json:"types"`
	DefaultAxis   string         `json:"default_axis"`
	Highlights    []struct {
		Path       string  `json:"path"`
		Title      string  `json:"title"`
		Type       string  `json:"type"`
		Confidence float64 `json:"confidence"`
		Impact     int     `json:"impact"`
	} `json:"highlights"`
	Links hal.LinkMap `json:"_links"`
}

func decodeStats(t *testing.T, body []byte) statsWire {
	t.Helper()
	var out statsWire
	require.NoError(t, json.Unmarshal(body, &out), string(body))
	return out
}

const statsURL = "/repos/alpha/branches/agent:test/stats"

func TestHandleHALStats_ReturnsHAL(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	a := m.Get("alpha")
	writeTypedFact(t, a, "kb/a/one.md", "One", fact.Observation, 0.9, nil, []string{"ai", "go"}, []string{"transformer"})
	writeTypedFact(t, a, "kb/a/two.md", "Two", fact.Observation, 0.8, nil, []string{"ai"}, nil)

	rec := getAPI(t, &Server{Manager: m}, statsURL)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	body := decodeStats(t, rec.Body.Bytes())
	require.Equal(t, 2, body.Total)
	require.Equal(t, 0.85, body.AvgConfidence)
	require.Equal(t, map[string]int{"ai": 2, "go": 1}, body.Domains)
	require.Equal(t, map[string]int{"transformer": 1}, body.Entities)
	require.Equal(t, APIBase+statsURL, body.Links["self"].Href)
}

func TestHandleHALStats_UnknownRepo_Returns404(t *testing.T) {
	m, _ := newRealStoreManager(t)

	rec := getAPI(t, &Server{Manager: m}, "/repos/missing/branches/main/stats")

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
}

// path= narrows the aggregates to the subtree. This regresses the bug where the
// handler always asked the store for the whole repo, so domain/entity counts
// in the right panel never updated when the user navigated into a subdirectory.
// It covers a prefix without a trailing slash and one with.
func TestHandleHALStats_PathQueryNarrowsTheAggregates(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	a := m.Get("alpha")
	writeTypedFact(t, a, "kb/meta/one.md", "One", fact.Observation, 0.9, nil, []string{"meta"}, nil)
	writeTypedFact(t, a, "kb/meta/two.md", "Two", fact.Observation, 0.9, nil, []string{"meta"}, nil)
	writeTypedFact(t, a, "kb/technology/three.md", "Three", fact.Observation, 0.9, nil, []string{"tech"}, nil)
	s := &Server{Manager: m}

	for _, tc := range []struct {
		query   string
		total   int
		domains map[string]int
	}{
		{"", 3, map[string]int{"meta": 2, "tech": 1}},
		{"?path=kb/meta", 2, map[string]int{"meta": 2}},
		{"?path=kb/technology/", 1, map[string]int{"tech": 1}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := getAPI(t, s, statsURL+tc.query)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			body := decodeStats(t, rec.Body.Bytes())
			require.Equal(t, tc.total, body.Total)
			require.Equal(t, tc.domains, body.Domains)
		})
	}
}

// The store's Stats read fails: the handler answers 500 carrying THAT error,
// and the fault fired for the Stats read and no other.
func TestHandleHALStats_StoreError_Returns500(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	injected := errors.New("injected stats read failure")
	var hits atomic.Int64
	t.Cleanup(svcs["alpha"].Search().SetReadFaultForTest(failIndexRead(index.ReadStats, injected, &hits)))

	rec := getAPI(t, &Server{Manager: m}, statsURL)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load(), "the Stats read must have reached the hook exactly once")
}

// An observation, plus a synthesis deriving from it: the synthesis is the only
// highlight (observations are the substrate and are excluded), carries its
// impact (one distinct derived-from target), and the observation-free
// out-degree makes impact the recommended axis.
func TestHandleHALStats_EmitsHighlightsTypesAndAxis(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	a := m.Get("alpha")
	writeTypedFact(t, a, "kb/o/a.md", "An observation", fact.Observation, 0.5, nil, []string{"x"}, nil)
	writeTypedFact(t, a, "kb/s/a.md", "A synthesis", fact.Synthesis, 0.8, []string{"kb/o/a.md"}, []string{"x"}, nil)

	rec := getAPI(t, &Server{Manager: m}, statsURL)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeStats(t, rec.Body.Bytes())
	require.Equal(t, "impact", body.DefaultAxis)
	require.Equal(t, map[string]int{"synthesis": 1, "observation": 1}, body.Types)
	require.Len(t, body.Highlights, 1)
	require.Equal(t, "kb/s/a.md", body.Highlights[0].Path)
	require.Equal(t, "A synthesis", body.Highlights[0].Title)
	require.Equal(t, "synthesis", body.Highlights[0].Type)
	require.Equal(t, 0.8, body.Highlights[0].Confidence)
	require.Equal(t, 1, body.Highlights[0].Impact)
}

// An empty highlights list and an empty type map serialize as [] and {}, never
// null, for a repo whose store is empty. (A store that is not live is no
// longer a zero StatsResult but the store-unavailable 503:
// TestReads_NoStore_Answer503.)
func TestHandleHALStats_EmptyHighlightsSerializeAsArrayNotNull(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	rec := getAPI(t, &Server{Manager: m}, statsURL)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"highlights":[]`)
	require.Contains(t, rec.Body.String(), `"types":{}`)
}

// axis= reaches the store and re-ranks the highlights: with impact as the
// recommended axis the high-impact synthesis leads; axis=confidence puts the
// high-confidence one first; an unrecognised axis falls back to the default.
func TestHandleHALStats_AxisReRanksTheHighlights(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	a := m.Get("alpha")
	writeTypedFact(t, a, "kb/o/a.md", "obs a", fact.Observation, 0.5, nil, nil, nil)
	writeTypedFact(t, a, "kb/o/b.md", "obs b", fact.Observation, 0.5, nil, nil, nil)
	writeTypedFact(t, a, "kb/s/impactful.md", "impactful", fact.Synthesis, 0.3, []string{"kb/o/a.md", "kb/o/b.md"}, nil, nil)
	writeTypedFact(t, a, "kb/s/confident.md", "confident", fact.Synthesis, 0.95, nil, nil, nil)
	s := &Server{Manager: m}

	for _, tc := range []struct{ query, first string }{
		{"", "kb/s/impactful.md"},
		{"?axis=impact", "kb/s/impactful.md"},
		{"?axis=confidence", "kb/s/confident.md"},
		{"?axis=bogus", "kb/s/impactful.md"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := getAPI(t, s, statsURL+tc.query)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			body := decodeStats(t, rec.Body.Bytes())
			require.Equal(t, "impact", body.DefaultAxis, "the recommendation does not follow the request")
			require.Len(t, body.Highlights, 2)
			require.Equal(t, tc.first, body.Highlights[0].Path)
		})
	}
}
