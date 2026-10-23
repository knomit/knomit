package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/index"
)

// The repo facts collection tests run over a REAL store: each test writes the
// facts it needs, and every query parameter is proved by what the listing
// returns (a corpus in which the filter keeps some facts and drops others), not
// by capturing the options the handler built. A failing read is made so through
// the owner's fault hook (index.Facade.SetReadFaultForTest).

const alphaFacts = "/repos/alpha/branches/agent:test/facts"

// factsRepoServer is a Server over one real repo, "alpha".
func factsRepoServer(t *testing.T) (*Server, *repos.RepoInstance) {
	t.Helper()
	m, _ := newRealStoreManager(t, "alpha")
	return &Server{Manager: m}, m.Get("alpha")
}

// factsWire mirrors the repo facts collection wire shape.
type factsWire struct {
	Count    int         `json:"count"`
	Links    hal.LinkMap `json:"_links"`
	Embedded struct {
		Facts []struct {
			Path        string      `json:"path"`
			Title       string      `json:"title"`
			Kind        string      `json:"kind"`
			Type        string      `json:"type"`
			Domain      []string    `json:"domain"`
			Entities    []string    `json:"entities"`
			CommittedAt int64       `json:"committed_at"`
			Operation   string      `json:"operation"`
			Links       hal.LinkMap `json:"_links"`
		} `json:"facts"`
	} `json:"_embedded"`
}

// getFacts GETs url from s and decodes a 200 collection.
func getFacts(t *testing.T, s *Server, url string) factsWire {
	t.Helper()
	rec := getAPI(t, s, url)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body factsWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body
}

// paths is the served fact paths, sorted (a listing of facts that share a
// commit has no order a test should depend on).
func (w factsWire) paths() []string {
	out := make([]string, 0, len(w.Embedded.Facts))
	for _, f := range w.Embedded.Facts {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

func TestHandleHALFactsCollection_ReturnsHALCollection(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeFactOp(t, ri, "kb/know/a.md", "Fact A", 0.9, "learn", nil)
	writeFactOp(t, ri, "kb/know/b.md", "Fact B", 0.9, "update", func(f *fact.Fact) { f.Type = fact.Hypothesis })

	rec := getAPI(t, s, alphaFacts)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body factsWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, 2, body.Count)
	require.Contains(t, body.Links, "self")
	require.Len(t, body.Embedded.Facts, 2)
	byPath := map[string]int{}
	for i, f := range body.Embedded.Facts {
		byPath[f.Path] = i
	}
	a, b := body.Embedded.Facts[byPath["kb/know/a.md"]], body.Embedded.Facts[byPath["kb/know/b.md"]]
	require.Equal(t, "Fact A", a.Title)
	require.Equal(t, "observation", a.Type)
	require.Equal(t, "learn", a.Operation)
	require.NotZero(t, a.CommittedAt)
	require.Equal(t, "Fact B", b.Title)
	require.Equal(t, "hypothesis", b.Type)
	require.Equal(t, "update", b.Operation)
	require.Equal(t, APIBase+"/repos/alpha/branches/agent:test/facts/kb/know/a.md", a.Links["self"].Href)
}

// limit and offset reach the store: one commit holds three facts (listed by
// path), so offset=1&limit=1 is exactly the middle one, and the page links name
// the neighbouring offsets.
func TestHandleHALFactsCollection_Pagination(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeFactsInOneCommit(t, ri,
		factSpec{path: "kb/know/a.md", title: "Fact A"},
		factSpec{path: "kb/know/b.md", title: "Fact B"},
		factSpec{path: "kb/know/c.md", title: "Fact C"})

	mid := getFacts(t, s, alphaFacts+"?limit=1&offset=1")
	require.Equal(t, 3, mid.Count, "count is the corpus total, not the page")
	require.Equal(t, []string{"kb/know/b.md"}, mid.paths())
	require.Contains(t, mid.Links["next"].Href, "offset=2")
	require.Contains(t, mid.Links["prev"].Href, "offset=0")

	// A page holding everything has neither neighbour.
	all := getFacts(t, s, alphaFacts+"?limit=3")
	require.Len(t, all.Embedded.Facts, 3)
	require.NotContains(t, all.Links, "next")
	require.NotContains(t, all.Links, "prev")
}

// A fact's domain and entities surface on the wire. The bridge's SessionStart
// hook filters by these fields client-side; without serialization it silently
// sees empty arrays and never renders the PROJECT PRINCIPLES block.
func TestHandleHALFactsCollection_SerializesDomainAndEntities(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/policy/p.md", "Principle", 0.9, func(f *fact.Fact) {
		f.Kind = fact.Pragmatic
		f.Type = fact.Policy
		f.Domain = []string{"global"}
		f.Entities = []string{"designer"}
	})

	body := getFacts(t, s, alphaFacts)

	require.Len(t, body.Embedded.Facts, 1)
	require.Equal(t, []string{"global"}, body.Embedded.Facts[0].Domain,
		"domain slice must appear in HAL response so the bridge can filter on it")
	require.Equal(t, []string{"designer"}, body.Embedded.Facts[0].Entities,
		"entities slice must appear in HAL response so the bridge can filter on it")
}

// Pragmatic entries carry "kind":"pragmatic" on the wire while epistemic
// entries elide the field (mirrors fact.Fact.MarshalJSON).
func TestHandleHALFactsCollection_KindSerialization(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/know/a.md", "Fact A", 0.9, nil)
	writeCosFact(t, ri, "kb/know/b.md", "Fact B", 0.9, func(f *fact.Fact) {
		f.Kind = fact.Pragmatic
		f.Type = fact.Policy
	})

	body := getFacts(t, s, alphaFacts)

	kinds := map[string]string{}
	for _, f := range body.Embedded.Facts {
		kinds[f.Path] = f.Kind
	}
	require.Equal(t, map[string]string{"kb/know/a.md": "", "kb/know/b.md": "pragmatic"}, kinds,
		"epistemic is elided, pragmatic is named")
	rec := getAPI(t, s, alphaFacts+"?kind=epistemic")
	require.NotContains(t, rec.Body.String(), `"kind"`, "the elided kind must be absent from the JSON, not empty")
}

// ?kind= and ?exclude_kind= each narrow the listing on their own: with one fact
// of each kind, dropping either parameter would return both.
func TestHandleHALFactsCollection_KindFilterNarrows(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/know/epi.md", "Epistemic", 0.9, nil)
	writeCosFact(t, ri, "kb/know/prag.md", "Pragmatic", 0.9, func(f *fact.Fact) {
		f.Kind = fact.Pragmatic
		f.Type = fact.Policy
	})

	require.Equal(t, []string{"kb/know/prag.md"}, getFacts(t, s, alphaFacts+"?kind=pragmatic").paths())
	require.Equal(t, []string{"kb/know/epi.md"}, getFacts(t, s, alphaFacts+"?kind=epistemic").paths())
	require.Equal(t, []string{"kb/know/epi.md"}, getFacts(t, s, alphaFacts+"?exclude_kind=pragmatic").paths())
	require.Equal(t, []string{"kb/know/prag.md"}, getFacts(t, s, alphaFacts+"?exclude_kind=epistemic").paths())
	require.Equal(t, []string{"kb/know/prag.md"}, getFacts(t, s, alphaFacts+"?kind=pragmatic&exclude_kind=epistemic").paths())
}

// ?origin= splits on commas and keeps exactly the listed origins: each of the
// two listed values lets its own fact through, the third origin stays out.
func TestHandleHALFactsCollection_OriginFilterNarrows(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/know/authored.md", "Authored", 0.9, nil)
	writeCosFact(t, ri, "kb/know/distilled.md", "Distilled", 0.9, func(f *fact.Fact) {
		f.Type = fact.Synthesis
		f.Origin = fact.Distilled
		f.Refs = []string{"kb/know/authored.md"}
	})
	writeCosFact(t, ri, "kb/know/discovered.md", "Discovered", 0.9, func(f *fact.Fact) {
		f.Type = fact.Hypothesis
		f.Origin = fact.Discovered
	})

	require.Equal(t, []string{"kb/know/discovered.md", "kb/know/distilled.md"},
		getFacts(t, s, alphaFacts+"?origin=distilled,discovered").paths())
	require.Equal(t, []string{"kb/know/authored.md"}, getFacts(t, s, alphaFacts+"?origin=authored").paths())
}

// ?topic=X is shorthand for the ontology-root prefix kb/X/.
func TestHandleHALFactsCollection_TopicNarrowsToTheOntologyRoot(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/invariants/a.md", "An invariant", 0.9, nil)
	writeCosFact(t, ri, "kb/gotchas/b.md", "A gotcha", 0.9, nil)
	// A fact whose path merely contains the topic is not under its root.
	writeCosFact(t, ri, "kb/other/invariants/c.md", "Not under the root", 0.9, nil)

	require.Equal(t, []string{"kb/invariants/a.md"}, getFacts(t, s, alphaFacts+"?topic=invariants").paths())
}

// An explicit ?path= wins over ?topic= so callers can override the convention.
func TestHandleHALFactsCollection_ExplicitPathBeatsTopic(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/invariants/a.md", "An invariant", 0.9, nil)
	writeCosFact(t, ri, "kb/gotchas/b.md", "A gotcha", 0.9, nil)

	require.Equal(t, []string{"kb/gotchas/b.md"},
		getFacts(t, s, alphaFacts+"?topic=invariants&path=kb/gotchas/").paths())
}

// ?path= is a prefix filter of its own (without a topic).
func TestHandleHALFactsCollection_PathNarrows(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/x/a.md", "Under x", 0.9, nil)
	writeCosFact(t, ri, "kb/y/b.md", "Under y", 0.9, nil)

	body := getFacts(t, s, alphaFacts+"?path=kb/x/")

	require.Equal(t, []string{"kb/x/a.md"}, body.paths())
	require.Equal(t, 1, body.Count)
}

// ?entity=X (singular, the canonical name the HAL template advertises) narrows
// to the facts carrying X.
func TestHandleHALFactsCollection_EntitySingularNarrows(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/know/a.md", "Verify", 0.9, func(f *fact.Fact) { f.Entities = []string{"Service.Verify"} })
	writeCosFact(t, ri, "kb/know/b.md", "Other", 0.9, func(f *fact.Fact) { f.Entities = []string{"Service.Other"} })

	require.Equal(t, []string{"kb/know/a.md"}, getFacts(t, s, alphaFacts+"?entity=Service.Verify").paths())
}

// ?entity=A&entities=B,C merges to all three values (singular is canonical,
// plural the alias), and a fact must carry every one of them. A fact with only
// A, or only B,C, is out, so dropping either parameter lets a wrong fact in.
func TestHandleHALFactsCollection_EntitySingularAndPluralMerge(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/know/abc.md", "All three", 0.9, func(f *fact.Fact) { f.Entities = []string{"A", "B", "C"} })
	writeCosFact(t, ri, "kb/know/a.md", "Only A", 0.9, func(f *fact.Fact) { f.Entities = []string{"A"} })
	writeCosFact(t, ri, "kb/know/bc.md", "Only B and C", 0.9, func(f *fact.Fact) { f.Entities = []string{"B", "C"} })

	require.Equal(t, []string{"kb/know/abc.md"}, getFacts(t, s, alphaFacts+"?entity=A&entities=B,C").paths())
}

// ?min_confidence=0.8 is parsed as 0.8 and keeps the facts at or above it.
func TestHandleHALFactsCollection_MinConfidenceNarrows(t *testing.T) {
	s, ri := factsRepoServer(t)
	conf := func(c float64) func(*fact.Fact) { return func(f *fact.Fact) { f.Confidence = c } }
	writeCosFact(t, ri, "kb/know/low.md", "Low", 0.5, conf(0.5))
	writeCosFact(t, ri, "kb/know/edge.md", "Edge", 0.8, conf(0.8))
	writeCosFact(t, ri, "kb/know/high.md", "High", 0.95, conf(0.95))

	require.Equal(t, []string{"kb/know/edge.md", "kb/know/high.md"}, getFacts(t, s, alphaFacts+"?min_confidence=0.8").paths())
}

// A non-numeric min_confidence is a 400, never silently zero.
func TestHandleHALFactsCollection_InvalidMinConfidence_Returns400(t *testing.T) {
	s, _ := factsRepoServer(t)

	rec := getAPI(t, s, alphaFacts+"?min_confidence=notanumber")

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleHALFactsCollection_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}

	rec := getAPI(t, s, "/repos/missing/branches/main/facts")

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// A failing store read is a 500 that carries the cause. The injected error is
// distinct and only the RecentFacts read is failed, so the 500 cannot come from
// another read, and the hit counter proves the read was reached once.
func TestHandleHALFactsCollection_StoreError_Returns500(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	s := &Server{Manager: m}
	writeCosFact(t, m.Get("alpha"), "kb/know/a.md", "Fact A", 0.9, nil)
	var hits atomic.Int64
	sentinel := errors.New("recent facts exploded: alpha")
	failMountRead(t, m, "alpha", index.ReadRecentFacts, sentinel, &hits)

	rec := getAPI(t, s, alphaFacts)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), sentinel.Error())
	require.EqualValues(t, 1, hits.Load())
}

// ?motifs= and ?motif_match= reach the store. The corpus separates the exact
// tier from the stem tier (a plural spelling matches only under stem) and from
// a fact outside the filter, so dropping the terms returns the stranger and
// dropping the tier loses the plural.
func TestHandleHALFactsCollection_MotifFilterNarrows(t *testing.T) {
	s, ri := factsRepoServer(t)
	motif := func(m string) func(*fact.Fact) { return func(f *fact.Fact) { f.Motifs = []string{m} } }
	writeCosFact(t, ri, "kb/a.md", "Zero value", 0.9, motif("zero-value-as-valid"))
	writeCosFact(t, ri, "kb/b.md", "Silent fallback", 0.9, motif("silent-fallback"))
	writeCosFact(t, ri, "kb/c.md", "Plural spelling", 0.9, motif("silent-fallbacks"))
	writeCosFact(t, ri, "kb/d.md", "Stranger", 0.9, motif("scope-creep"))

	const terms = "?motifs=zero-value-as-valid,silent-fallback"
	require.Equal(t, []string{"kb/a.md", "kb/b.md", "kb/c.md"},
		getFacts(t, s, alphaFacts+terms+"&motif_match=stem").paths())
	require.Equal(t, []string{"kb/a.md", "kb/b.md"}, getFacts(t, s, alphaFacts+terms).paths(),
		"without a tier the filter is exact")
	require.Equal(t, []string{"kb/a.md", "kb/b.md"}, getFacts(t, s, alphaFacts+terms+"&motif_match=exact").paths())
}

// A loose tier is refused at the edge, before any store call (C3/MN6): the
// store is not read at all, and a permitted tier does read it once, so the
// counter could have moved.
func TestHandleHALFactsCollection_LooseMotifTierRejected(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	s := &Server{Manager: m}
	reads := countMountReads(t, m, []string{"alpha"}, "RecentFacts")

	rec := getAPI(t, s, alphaFacts+"?motifs=a-b&motif_match=token-1")

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.EqualValues(t, 0, reads["alpha"]["RecentFacts"].Load(), "the store was read despite the rejection")

	rec = getAPI(t, s, alphaFacts+"?motifs=a-b&motif_match=token-2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.EqualValues(t, 1, reads["alpha"]["RecentFacts"].Load())
}

// Rows carry their motifs, and a motif-free row omits the key entirely.
func TestHandleHALFactsCollection_MotifsOnTheWire(t *testing.T) {
	s, ri := factsRepoServer(t)
	writeCosFact(t, ri, "kb/a.md", "Carries a motif", 0.9, func(f *fact.Fact) { f.Motifs = []string{"a-b"} })
	writeCosFact(t, ri, "kb/b.md", "Carries none", 0.9, nil)

	rec := getAPI(t, s, alphaFacts)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Embedded struct {
			Facts []map[string]any `json:"facts"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Len(t, view.Embedded.Facts, 2)
	for _, f := range view.Embedded.Facts {
		switch f["path"] {
		case "kb/a.md":
			require.Equal(t, []any{"a-b"}, f["motifs"])
		case "kb/b.md":
			_, present := f["motifs"]
			require.False(t, present, "motif-free row must omit the key entirely")
		default:
			t.Fatalf("unexpected row %v", f["path"])
		}
	}
}
