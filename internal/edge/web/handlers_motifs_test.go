package web

import (
	"context"
	"encoding/json"
	"errors"
	"knomit/internal/edge/web/httpx"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/vocab"
)

// The motif vocabulary handlers read REAL stores. A vocabulary is what the
// store derives from the facts it holds (mechanical clusters by grouping key),
// plus what a review session records on top of it: judge merges, applied by an
// alias rebuild, and definitions. The helpers below make those records the way
// the review session does, through the store's own vocab.MotifIndex; a read is
// failed through vocab.MotifIndex.SetReadFaultForTest.

// withMotifIndex runs fn with ri's real motif index and the branch ri's facts
// are written to.
func withMotifIndex(t testing.TB, ri *repos.RepoInstance, fn func(mi *vocab.MotifIndex, branch string)) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "the test repo must have a live store")
		fn(svc.Motifs(), ri.AgentBranch())
	}))
}

// judgeMerge records that a judge merged the clusters of spellings a and b on
// ri, and rebuilds ri's alias table so the merge takes effect (a recorded merge
// binds only from the next rebuild).
func judgeMerge(t testing.TB, ri *repos.RepoInstance, a, b, rationale string) {
	t.Helper()
	withMotifIndex(t, ri, func(mi *vocab.MotifIndex, branch string) {
		require.NoError(t, mi.RecordJudgeMerge(context.Background(), branch, a, b, rationale))
	})
	rebuildAliases(t, ri)
}

// rebuildAliases recomputes ri's alias table from its live facts and recorded
// judge merges, as a review session does.
func rebuildAliases(t testing.TB, ri *repos.RepoInstance) {
	t.Helper()
	withMotifIndex(t, ri, func(mi *vocab.MotifIndex, branch string) {
		require.NoError(t, mi.RebuildAliases(context.Background(), branch))
	})
}

// defineMotif stores def as the definition of clusterKey on ri. A current one
// is stamped with the cluster's membership as it stands; a stale one with a
// membership the cluster does not have, which is how a definition goes stale
// when a merge changes its cluster. Call it after the last alias rebuild: a
// rebuild retires definitions whose cluster it no longer finds.
func defineMotif(t testing.TB, ri *repos.RepoInstance, clusterKey, def string, stale bool) {
	t.Helper()
	stamp := vocab.DefinitionStamp{}
	if stale {
		stamp = vocab.DefinitionStamp{Members: "a-membership-this-cluster-never-had", Known: true}
	}
	withMotifIndex(t, ri, func(mi *vocab.MotifIndex, branch string) {
		require.NoError(t, mi.PutDefinition(context.Background(), branch, clusterKey, def, stamp))
	})
}

// failMotifRead fails exactly the named motif read of ri's store with err, and
// fails no other read. hits counts how often the named read reached the hook;
// it is atomic because a lens reads its mounts from more than one goroutine.
func failMotifRead(t testing.TB, ri *repos.RepoInstance, read vocab.ReadName, err error, hits *atomic.Int64) {
	t.Helper()
	withMotifIndex(t, ri, func(mi *vocab.MotifIndex, _ string) {
		t.Cleanup(mi.SetReadFaultForTest(func(got vocab.ReadName) error {
			if got != read {
				return nil
			}
			hits.Add(1)
			return err
		}))
	})
}

// requireFailedWith asserts rec is the 500 an injected store error produces,
// carrying that error's own text (a bare 500 also passes for a read that failed
// for some other reason), and that the faulted read was reached exactly once.
func requireFailedWith(t *testing.T, rec *httptest.ResponseRecorder, sentinel error, hits *atomic.Int64) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), sentinel.Error(), "the problem must carry the injected error")
	require.EqualValues(t, 1, hits.Load(), "the faulted read must be reached exactly once")
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
		MintToLinkRatio   float64 `json:"mint_to_link_ratio"`

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
		Motifs []motifsCollectionEntry `json:"motifs"`
	} `json:"_embedded"`
}

// motifsCollectionEntry is one cluster row of the vocabulary collection.
type motifsCollectionEntry struct {
	ClusterKey      string   `json:"cluster_key"`
	Canonical       string   `json:"canonical"`
	Members         []string `json:"members"`
	DF              int      `json:"df"`
	DFTotal         int      `json:"df_total"`
	Definition      string   `json:"definition"`
	DefinitionState string   `json:"definition_state"`
	Links           struct {
		Self struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"_links"`
}

// keys is the page's cluster keys, in page order.
func (b motifsCollectionBody) keys() []string {
	out := make([]string, 0, len(b.Embedded.Motifs))
	for _, e := range b.Embedded.Motifs {
		out = append(out, e.ClusterKey)
	}
	return out
}

// entry is the page's row for key; it fails the test when there is none.
func (b motifsCollectionBody) entry(t *testing.T, key string) motifsCollectionEntry {
	t.Helper()
	for _, e := range b.Embedded.Motifs {
		if e.ClusterKey == key {
			return e
		}
	}
	t.Fatalf("no %q row; got %v", key, b.keys())
	return motifsCollectionEntry{}
}

// motifsRepoServer serves repo "alpha" over a REAL store (agent branch
// agent/test, addressed as agent:test) and returns the router and the repo.
func motifsRepoServer(t *testing.T) (http.Handler, *repos.RepoInstance) {
	t.Helper()
	m, _ := newRealStoreManager(t, "alpha")
	return (&Server{Manager: m}).NewAPIRouter(), m.Get("alpha")
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

// withKindMotifs makes a fact pragmatic (a policy) or leaves it epistemic, and sets
// its motifs.
func withKindMotifs(pragmatic bool, ms ...string) func(*fact.Fact) {
	return func(f *fact.Fact) {
		if pragmatic {
			f.Kind, f.Type = fact.Pragmatic, fact.Policy
		}
		f.Motifs = ms
	}
}

// writeRepoVocabulary writes the vocabulary the collection tests read into ri,
// in one commit, then records what a review session would on top of it:
//
//	cluster          members                             df  under kb/decisions/
//	config-drift     config-drift (4), configuration-drifts (2)  6  2   judge-merged
//	fallback-silent  silent-fallback                      2  1   one carrier pragmatic
//	retry-storm      retry-storm                          1  0   DISTILLED only
//	creep-scope      scope-creep                          1  0
//	cache-stale      stale-cache                          1  0
//
// config-drift has a CURRENT definition, fallback-silent a STALE one, the rest
// none. Every number the collection reports is distinct where the handler
// could confuse two sources: count 5 (every origin), and over AUTHORED facts
// clusters 4, recurring 2, links 6 (5+1), epistemic recurring 1 (silent-
// fallback has one epistemic carrier). Mints is one per cluster in any real
// corpus, so it always equals clusters.
func writeRepoVocabulary(t *testing.T, ri *repos.RepoInstance) {
	t.Helper()
	writeFactsInOneCommit(t, ri,
		plainFact("kb/decisions/cd0.md", "drift 0", withMotifs("config-drift")),
		plainFact("kb/decisions/cd1.md", "drift 1", withMotifs("config-drift")),
		plainFact("kb/other/cd2.md", "drift 2", withMotifs("config-drift")),
		plainFact("kb/other/cd3.md", "drift 3", withMotifs("config-drift")),
		plainFact("kb/other/cr0.md", "drifts 0", withMotifs("configuration-drifts")),
		plainFact("kb/other/cr1.md", "drifts 1", withMotifs("configuration-drifts")),
		plainFact("kb/decisions/sf0.md", "fallback 0", withKindMotifs(true, "silent-fallback")),
		plainFact("kb/other/sf1.md", "fallback 1", withMotifs("silent-fallback")),
		plainFact("kb/other/rs.md", "retry", func(f *fact.Fact) {
			f.Type, f.Origin = fact.Synthesis, fact.Distilled
			f.Motifs = []string{"retry-storm"}
		}),
		plainFact("kb/other/sc.md", "scope", withMotifs("scope-creep")),
		plainFact("kb/other/st.md", "stale", withMotifs("stale-cache")))
	judgeMerge(t, ri, "config-drift", "configuration-drifts", "same mechanism")
	defineMotif(t, ri, "config-drift", "Configured state diverges from applied state.", false)
	defineMotif(t, ri, "fallback-silent", "A component keeps serving after a dependency fails.", true)
}

// vocabularyServer is motifsRepoServer holding writeRepoVocabulary.
func vocabularyServer(t *testing.T) http.Handler {
	t.Helper()
	r, ri := motifsRepoServer(t)
	writeRepoVocabulary(t, ri)
	return r
}

const alphaMotifs = "/repos/alpha/branches/agent:test/motifs"

func TestHandleHALMotifs_ReturnsRankedCollection(t *testing.T) {
	body := decodeMotifs(t, getMotifs(t, vocabularyServer(t), alphaMotifs))

	require.Equal(t, 5, body.Count, "every cluster, every origin")
	// df-desc, ties by canonical: the three df-1 clusters order as retry-storm,
	// scope-creep, stale-cache.
	require.Equal(t, []string{"config-drift", "fallback-silent", "retry-storm", "creep-scope", "cache-stale"}, body.keys())
	wantStates := []string{"current", "stale", "missing", "missing", "missing"}
	for i, e := range body.Embedded.Motifs {
		if e.DefinitionState != wantStates[i] {
			t.Errorf("entry %d (%s) definition_state: got %q, want %q", i, e.ClusterKey, e.DefinitionState, wantStates[i])
		}
		if want := alphaMotifs + "/" + e.ClusterKey; !strings.HasSuffix(e.Links.Self.Href, want) {
			t.Errorf("entry %d self: got %q, want suffix %q", i, e.Links.Self.Href, want)
		}
	}
	// Humans read the canonical; the URL keys on the cluster key (C1). The
	// judge merge made one cluster of two spellings that share no grouping key.
	drift := body.Embedded.Motifs[0]
	require.Equal(t, "config-drift", drift.Canonical, "the higher-df spelling (4 > 2)")
	require.Equal(t, []string{"config-drift", "configuration-drifts"}, drift.Members)
	require.Equal(t, 6, drift.DF)
	require.Equal(t, 6, drift.DFTotal, "unscoped: df_total is df")
	require.Equal(t, "Configured state diverges from applied state.", drift.Definition)
	require.Equal(t, "A component keeps serving after a dependency fails.", body.Embedded.Motifs[1].Definition,
		"a stale definition is still served")
	require.Equal(t, "", body.Embedded.Motifs[2].Definition, "a missing definition is empty")

	require.Equal(t, 4, body.Health.AuthoredClusters, "the distilled retry-storm is not authored")
	require.Equal(t, 2, body.Health.AuthoredRecurring)
	require.Equal(t, 0.5, body.Health.RecurrenceRate)
}

// Two requests, the name sort first: the default df-desc order must survive a
// name-sorted request (the sort orders a copy of what the store returned).
func TestHandleHALMotifs_SortName(t *testing.T) {
	r := vocabularyServer(t)

	body := decodeMotifs(t, getMotifs(t, r, alphaMotifs+"?sort=name"))
	var canon []string
	for _, e := range body.Embedded.Motifs {
		canon = append(canon, e.Canonical)
	}
	require.Equal(t, []string{"config-drift", "retry-storm", "scope-creep", "silent-fallback", "stale-cache"}, canon)

	body = decodeMotifs(t, getMotifs(t, r, alphaMotifs))
	require.Equal(t, []string{"config-drift", "fallback-silent", "retry-storm", "creep-scope", "cache-stale"}, body.keys(),
		"the default order after a ?sort=name request")

	if rec := getMotifs(t, r, alphaMotifs+"?sort=bogus"); rec.Code != http.StatusBadRequest {
		t.Errorf("sort=bogus: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALMotifs_NarrowByQ(t *testing.T) {
	r := vocabularyServer(t)

	body := decodeMotifs(t, getMotifs(t, r, alphaMotifs+"?q=FALLBACK"))
	require.Equal(t, 1, body.Count, "count reflects the narrowed total")
	require.Equal(t, []string{"fallback-silent"}, body.keys())

	// The definition, not any member spelling, is what "diverges" hits.
	body = decodeMotifs(t, getMotifs(t, r, alphaMotifs+"?q=diverges"))
	require.Equal(t, 1, body.Count)
	require.Equal(t, []string{"config-drift"}, body.keys(), "definition text must be searchable")
}

func TestHandleHALMotifs_Paging(t *testing.T) {
	r := vocabularyServer(t)

	body := decodeMotifs(t, getMotifs(t, r, alphaMotifs+"?limit=2"))
	require.Equal(t, []string{"config-drift", "fallback-silent"}, body.keys())
	require.Equal(t, 5, body.Count, "the total, not the page")
	require.NotNil(t, body.Links.Next, "next link missing while clusters remain")
	require.Contains(t, body.Links.Next.Href, "offset=2")
	require.Nil(t, body.Links.Prev, "prev must be absent on the first page")

	body = decodeMotifs(t, getMotifs(t, r, alphaMotifs+"?limit=2&offset=4"))
	require.Equal(t, []string{"cache-stale"}, body.keys(), "the last page")
	require.Nil(t, body.Links.Next, "next must be absent on the last page")
	require.NotNil(t, body.Links.Prev, "prev link missing on a later page")
	require.Contains(t, body.Links.Prev.Href, "offset=2")

	// The ceiling is HARD, not a clamp (the params.go house rule): a client
	// that asked for 500 and silently got 200 rows cannot tell that from "there
	// were only 200".
	for _, bad := range []string{"limit=201", "limit=500", "limit=0", "limit=x", "offset=-1", "offset=x"} {
		if rec := getMotifs(t, r, alphaMotifs+"?"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, rec.Code)
		}
	}
	// A refusal that says only "invalid" makes the caller guess the bound; the
	// ceiling refusal names the number, as limitParam's does.
	rec := getMotifs(t, r, alphaMotifs+"?limit=201")
	if !containsSub(rec.Body.String(), "limit must not exceed 200") {
		t.Errorf("over-ceiling detail must name the ceiling: %s", rec.Body.String())
	}
}

func TestHandleHALMotifs_EmptyVocabulary(t *testing.T) {
	r, ri := motifsRepoServer(t)
	// A corpus with facts but no motifs at all: the vocabulary is empty.
	writeCosFact(t, ri, "kb/a.md", "no motifs", 0.9, nil)

	rec := getMotifs(t, r, alphaMotifs)
	body := decodeMotifs(t, rec)
	require.Equal(t, 0, body.Count)
	require.Equal(t, 0, body.Health.AuthoredClusters)
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

// Each of the collection's three reads fails the request with the store's own
// error, and the handler stops at the read that failed.
func TestHandleHALMotifs_StoreErrorIs500(t *testing.T) {
	for _, read := range []vocab.ReadName{vocab.ReadClustersUnder, vocab.ReadVocabularyHealthUnder, vocab.ReadDefinitions} {
		t.Run(string(read), func(t *testing.T) {
			r, ri := motifsRepoServer(t)
			writeRepoVocabulary(t, ri)
			sentinel := errors.New("motif read " + string(read) + " on fire")
			var hits atomic.Int64
			failMotifRead(t, ri, read, sentinel, &hits)

			requireFailedWith(t, getMotifs(t, r, alphaMotifs), sentinel, &hits)
		})
	}
}

// ── cluster detail ────────────────────────────────────────────────────────────

// motifDetailBody mirrors the cluster detail wire shape.
type motifDetailBody struct {
	ClusterKey      string   `json:"cluster_key"`
	Canonical       string   `json:"canonical"`
	Members         []string `json:"members"`
	DF              int      `json:"df"`
	DFTotal         int      `json:"df_total"`
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

// aliasesByMotif is the detail's alias rows as motif -> "method/rationale".
func (b motifDetailBody) aliasesByMotif() map[string]string {
	out := map[string]string{}
	for _, a := range b.Aliases {
		out[a.Motif] = a.Method + "/" + a.Rationale
	}
	return out
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

// withMotifs sets a fact's motif spellings.
func withMotifs(ms ...string) func(*fact.Fact) { return func(f *fact.Fact) { f.Motifs = ms } }

// writeDriftCarriers writes the `config-drift` cluster into ri as a review
// session would leave it, and then one fact written AFTER that session:
//
//   - one commit: kb/b.md carries `configuration-drifts` (a policy), kb/c.md
//     `config-drift`, kb/d.md both, kb/x.md is a stranger (`scope-creep`);
//   - a judge merge of `config-drift` and `configuration-drifts` (which share
//     no grouping key), the alias rebuild that applies it, and a current
//     definition;
//   - a later commit: kb/a.md, the newest carrier, spells it `config-drifts`.
//     Its grouping key is `config-drift`, so the cluster lists it as a member,
//     but the alias table has no row for it yet: at the exact tier it resolves
//     only to itself, so the carriers reach it only because every member
//     spelling is passed (C2).
//
// Four carriers, newest first a, b, c, d; df 4.
func writeDriftCarriers(t *testing.T, ri *repos.RepoInstance) {
	t.Helper()
	writeFactsInOneCommit(t, ri,
		plainFact("kb/b.md", "Older carrier", withKindMotifs(true, "configuration-drifts")),
		plainFact("kb/c.md", "Third carrier", withMotifs("config-drift")),
		plainFact("kb/d.md", "Fourth carrier", withMotifs("config-drift", "configuration-drifts")),
		plainFact("kb/x.md", "Stranger", withMotifs("scope-creep")))
	judgeMerge(t, ri, "config-drift", "configuration-drifts", "same mechanism")
	defineMotif(t, ri, "config-drift", "Configured state diverges from applied state.", false)
	awaitNextSecond()
	writeCosFact(t, ri, "kb/a.md", "Newest carrier", 0.9, withMotifs("config-drifts"))
}

func TestHandleHALMotifCluster_ByClusterKey(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	r, ri := motifsRepoServer(t)
	writeDriftCarriers(t, ri)
	body := decodeMotifDetail(t, getMotifs(t, r, alphaMotifs+"/config-drift"))

	require.Equal(t, "config-drift", body.ClusterKey)
	// The rebuild elected config-drift (df 2 each, the lexicographic tiebreak);
	// the spelling written since has no say until the next rebuild.
	require.Equal(t, "config-drift", body.Canonical)
	require.Equal(t, []string{"config-drift", "config-drifts", "configuration-drifts"}, body.Members)
	require.Equal(t, 4, body.DF)
	// The spec requires df_total on every cluster body, the detail included —
	// MotifClusterDetail composes MotifClusterEntry via allOf. The detail reads
	// the branch unscoped, so it is df.
	require.Equal(t, 4, body.DFTotal)
	require.Equal(t, "current", body.DefinitionState)
	require.Equal(t, "Configured state diverges from applied state.", body.Definition)

	// The carriers ARE the pivot query: every member spelling at the exact tier
	// (so the stranger is out, and each spelling's own carriers are in, the one
	// the alias table does not know yet included), newest first, under the
	// default preview limit of 20.
	var carriers []string
	for _, c := range body.Carriers {
		carriers = append(carriers, c.Title)
	}
	require.Equal(t, []string{"Newest carrier", "Older carrier", "Third carrier", "Fourth carrier"}, carriers)
	if body.Carriers[0].Path != "kb/a.md" || body.Carriers[1].Type != "policy" {
		t.Errorf("carrier rows: got %+v", body.Carriers)
	}
	if body.Carriers[0].CommittedAt <= body.Carriers[1].CommittedAt {
		t.Errorf("carriers are not newest-first: %+v", body.Carriers)
	}
	require.Equal(t, 4, body.CarrierCount)

	// One alias row per member: the merged spellings carry the judge's audit
	// trail, the spelling written since the rebuild has none yet.
	require.Equal(t, map[string]string{
		"config-drift":         "judge/same mechanism",
		"configuration-drifts": "judge/same mechanism",
		"config-drifts":        "/",
	}, body.aliasesByMotif())

	require.True(t, strings.HasSuffix(body.Links.Self.Href, "/motifs/config-drift"), body.Links.Self.Href)
	wantFacts := "/facts?motif_match=exact&motifs=config-drift%2Cconfig-drifts%2Cconfiguration-drifts"
	require.True(t, strings.HasSuffix(body.Links.Facts.Href, wantFacts), "facts link: got %q, want suffix %q", body.Links.Facts.Href, wantFacts)

	// The preview can never disagree with the listing it links to: the facts
	// link, followed, lists exactly the carriers the detail previewed.
	rec := getMotifs(t, r, strings.TrimPrefix(body.Links.Facts.Href, httpx.APIBase))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var linked factsWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &linked))
	var previewed []string
	for _, c := range body.Carriers {
		previewed = append(previewed, c.Path)
	}
	sort.Strings(previewed)
	require.Equal(t, previewed, linked.paths())
	require.Equal(t, body.CarrierCount, linked.Count)
}

// A spelling resolves through the store's ClusterKey. `configuration-drifts`
// is no cluster key, and neither is its own grouping key
// (`configuration-drift`): only the alias row the judge merge left reaches the
// cluster. A spelling the alias table does not know degrades to its grouping
// key (C2), which for `config-drifts` is the cluster key.
func TestHandleHALMotifCluster_BySpelling(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	r, ri := motifsRepoServer(t)
	writeDriftCarriers(t, ri)

	for _, spelling := range []string{"configuration-drifts", "config-drifts"} {
		body := decodeMotifDetail(t, getMotifs(t, r, alphaMotifs+"/"+spelling))
		require.Equal(t, "config-drift", body.ClusterKey, spelling)
		// The self link canonicalizes to the cluster key regardless of how the
		// reader arrived (C1).
		require.True(t, strings.HasSuffix(body.Links.Self.Href, "/motifs/config-drift"), "%s: self %q", spelling, body.Links.Self.Href)
	}
}

// A definition is keyed by the STABLE cluster key, never by the displayed
// canonical: the canonical flips as usage shifts, and a definition keyed to it
// would be orphaned by a change that meant nothing. In writeRepoVocabulary the
// cluster `fallback-silent` is displayed as `silent-fallback`, so a detail that
// read or looked up its definition by canonical would find none ("missing").
// Addressed by key and by spelling, the detail serves the stale definition the
// store holds under the key.
func TestHandleHALMotifCluster_DefinitionIsKeyedByClusterKey(t *testing.T) {
	r := vocabularyServer(t)
	for _, addr := range []string{"fallback-silent", "silent-fallback"} {
		body := decodeMotifDetail(t, getMotifs(t, r, alphaMotifs+"/"+addr))
		require.Equal(t, "fallback-silent", body.ClusterKey, addr)
		require.Equal(t, "silent-fallback", body.Canonical, "%s: fixture: the key differs from the canonical", addr)
		require.Equal(t, "A component keeps serving after a dependency fails.", body.Definition, addr)
		require.Equal(t, "stale", body.DefinitionState, addr)
	}
}

func TestHandleHALMotifCluster_UnknownIs404(t *testing.T) {
	r, ri := motifsRepoServer(t)
	writeCosFact(t, ri, "kb/a.md", "carrier", 0.9, withMotifs("config-drift"))
	rec := getMotifs(t, r, alphaMotifs+"/never-heard-of-it")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// An unrebuilt corpus has an empty alias table: every spelling is its own
// singleton cluster, keyed by its grouping key. That degradation is a 200 with
// one member and no audit rows — never a 404 (C2). The spelling is not the key
// (`silent-fallback` keys as `fallback-silent`), so the detail resolves it.
func TestHandleHALMotifCluster_SingletonOnUnrebuiltCorpus(t *testing.T) {
	r, ri := motifsRepoServer(t)
	writeCosFact(t, ri, "kb/a.md", "carrier", 0.9, withMotifs("silent-fallback"))

	rec := getMotifs(t, r, alphaMotifs+"/silent-fallback")
	body := decodeMotifDetail(t, rec)

	require.Equal(t, "fallback-silent", body.ClusterKey)
	require.Equal(t, []string{"silent-fallback"}, body.Members)
	require.Equal(t, "missing", body.DefinitionState)
	require.Equal(t, map[string]string{"silent-fallback": "/"}, body.aliasesByMotif(), "one member with no audit row")
	if !containsSub(rec.Body.String(), `"aliases":[`) {
		t.Errorf("aliases must serialize as an array, not null: %s", rec.Body.String())
	}
}

func TestHandleHALMotifCluster_CarriersLimitParam(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	r, ri := motifsRepoServer(t)
	writeDriftCarriers(t, ri)

	// limit bounds the PREVIEW, never the count: two of the four carriers, the
	// newest first, and carrier_count still names all four.
	body := decodeMotifDetail(t, getMotifs(t, r, alphaMotifs+"/config-drift?limit=2"))
	require.Len(t, body.Carriers, 2)
	require.Equal(t, "Newest carrier", body.Carriers[0].Title)
	require.Equal(t, 4, body.CarrierCount, "bounding the preview must never bound the count")
	// The ceiling itself is a valid limit.
	decodeMotifDetail(t, getMotifs(t, r, alphaMotifs+"/config-drift?limit=100"))

	// The carrier ceiling is hard too: over it is a 400, never a quiet 100.
	for _, bad := range []string{"limit=101", "limit=500", "limit=0", "limit=x"} {
		if rec := getMotifs(t, r, alphaMotifs+"/config-drift?"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, rec.Code)
		}
	}
	rec := getMotifs(t, r, alphaMotifs+"/config-drift?limit=101")
	if !containsSub(rec.Body.String(), "limit must not exceed 100") {
		t.Errorf("over-ceiling detail must name the ceiling: %s", rec.Body.String())
	}
}

// Each of the detail's vocabulary reads fails the request with the store's
// own error.
func TestHandleHALMotifCluster_StoreErrorIs500(t *testing.T) {
	for _, read := range []vocab.ReadName{vocab.ReadClustersUnder, vocab.ReadDefinitions, vocab.ReadAliasRows} {
		t.Run(string(read), func(t *testing.T) {
			r, ri := motifsRepoServer(t)
			writeFactsInOneCommit(t, ri, plainFact("kb/c.md", "carrier", withMotifs("config-drift")))
			sentinel := errors.New("motif read " + string(read) + " on fire")
			var hits atomic.Int64
			failMotifRead(t, ri, read, sentinel, &hits)

			requireFailedWith(t, getMotifs(t, r, alphaMotifs+"/config-drift"), sentinel, &hits)
		})
	}
}

// An offset near MaxInt passes the `n < 0` parse guard, and then offset+limit
// WRAPS NEGATIVE — clearing every `< total` comparison written as a sum and
// reaching the slice expression as a negative bound. The result is a bounds
// panic on an unauthenticated GET, so the arithmetic must never form the sum.
// Mirrors TestLensFanoutDepth_OverflowingOffsetStillClamps.
func TestHandleHALMotifs_OverflowingOffsetIsAnEmptyPage(t *testing.T) {
	r := vocabularyServer(t)

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
			rec := getMotifs(t, r, alphaMotifs+"?"+tc.query)
			body := decodeMotifs(t, rec)
			if len(body.Embedded.Motifs) != 0 {
				t.Errorf("page: got %d entries, want none past the end", len(body.Embedded.Motifs))
			}
			if body.Count != 5 {
				t.Errorf("count: got %d, want the unnarrowed total 5", body.Count)
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
	// writeRepoVocabulary makes every field a distinct number, none of them 5
	// (the cluster count), so each assertion below passes only if that field is
	// wired to its own source. authored_mints is the exception the store
	// imposes: one mint per cluster, so it always equals authored_clusters, and
	// a swap of those two is invisible on any real corpus.
	r := vocabularyServer(t)

	body := decodeMotifs(t, getMotifs(t, r, alphaMotifs))
	require.Equal(t, 5, body.Count, "every cluster, every origin")
	// One assertion per field, each naming its own expected value: a single
	// combined check reports "health: got {...}" and leaves the reader to work
	// out which field moved.
	for _, f := range []struct {
		name string
		got  int
		want int
	}{
		{"authored_clusters", body.Health.AuthoredClusters, 4},
		{"authored_recurring", body.Health.AuthoredRecurring, 2},
		{"authored_mints", body.Health.AuthoredMints, 4},
		{"authored_links", body.Health.AuthoredLinks, 6},
		// authored AND epistemic — VocabularyHealth computes it with a kind
		// CASE inside the same origin='authored' filter, so it narrows by kind
		// as well as origin. Distinct from authored_recurring above precisely
		// so that wiring this to Recurring fails.
		{"authored_epistemic_recurring", body.Health.AuthoredEpistemicRecurring, 1},
	} {
		if f.got != f.want {
			t.Errorf("%s: got %d, want %d", f.name, f.got, f.want)
		}
	}
	require.Equal(t, 0.5, body.Health.RecurrenceRate, "2 of 4")
	require.InDelta(t, 4.0/6.0, body.Health.MintToLinkRatio, 1e-9, "4 mints over 6 links")

	// ?q= narrows count. It does NOT narrow health — the health block is a
	// corpus-level diagnostic, not a description of the page.
	narrowed := decodeMotifs(t, getMotifs(t, r, alphaMotifs+"?q=fallback"))
	require.Equal(t, 1, narrowed.Count)
	require.Equal(t, 4, narrowed.Health.AuthoredClusters, "authored_clusters under ?q=: the unnarrowed value")
	require.Equal(t, 6, narrowed.Health.AuthoredLinks, "authored_links under ?q=: the unnarrowed value")

	// The unqualified names must be gone from the wire: a reader who sees
	// "clusters" beside "count" reads them as the same population.
	raw := getMotifs(t, r, alphaMotifs).Body.String()
	for _, gone := range []string{`"clusters"`, `"recurring"`, `"mints"`, `"links"`, `"epistemic_recurring"`} {
		if containsSub(raw, gone) {
			t.Errorf("unqualified health field %s is still on the wire: %s", gone, raw)
		}
	}
}

// ?path= is SCOPE and reaches the store, for the list AND for the health block:
// the two sit in one row of the panel, and counting them over different
// populations is what makes a reader read the pair as a bug. Under
// kb/decisions/ only two clusters have carriers, and the health there is its
// own (clusters 2, recurring 1, links 1), not the branch's (4, 2, 6).
func TestHandleHALMotifs_PathScopesListAndHealth(t *testing.T) {
	body := decodeMotifs(t, getMotifs(t, vocabularyServer(t), alphaMotifs+"?path=kb%2Fdecisions"))

	require.Equal(t, 2, body.Count)
	require.Equal(t, []string{"config-drift", "fallback-silent"}, body.keys())
	require.Equal(t, 2, body.Health.AuthoredClusters, "a branch-wide strip beside a scoped list reports on facts the reader cannot see")
	require.Equal(t, 1, body.Health.AuthoredRecurring)
	require.Equal(t, 1, body.Health.AuthoredLinks)
}

// df and df_total are both on the wire, always. The row shows what is HERE; the
// pivot it opens drops the path, and df_total is how the row can say so.
func TestHandleHALMotifs_CarriesBothCounts(t *testing.T) {
	body := decodeMotifs(t, getMotifs(t, vocabularyServer(t), alphaMotifs+"?path=kb%2Fdecisions"))

	for key, want := range map[string][2]int{"config-drift": {2, 6}, "fallback-silent": {1, 2}} {
		e := body.entry(t, key)
		require.Equal(t, want, [2]int{e.DF, e.DFTotal}, "%s: df and df_total", key)
	}
}

// No ?path= is an unscoped read, not a defaulted one: the store's own empty
// prefix is what "the whole branch" means, and the handler must not invent a
// substitute for it. Every carrier counts, wherever it lives.
func TestHandleHALMotifs_NoPathPassesEmptyPrefix(t *testing.T) {
	body := decodeMotifs(t, getMotifs(t, vocabularyServer(t), alphaMotifs))
	require.Equal(t, 5, body.Count)
	e := body.entry(t, "config-drift")
	require.Equal(t, [2]int{6, 6}, [2]int{e.DF, e.DFTotal})
	require.Equal(t, 4, body.Health.AuthoredClusters)
}
