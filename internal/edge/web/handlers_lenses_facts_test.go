package web

import (
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
)

// The lens union facts tests (GET /lenses/{lens}/facts) run over REAL stores:
// the mounts are preset repos, each test writes the facts it needs, and every
// query parameter, depth and count is proved by what the union serves from that
// corpus. Recency needs distinct commit stamps, which have one-second
// resolution: a test that needs a total order writes one commit per group and
// calls awaitNextSecond between them; facts that must tie share a commit and are
// listed by path.

// titles is the served titles, in served order.
func (b lensFactsBody) titles() []string {
	out := make([]string, 0, len(b.Facts))
	for _, f := range b.Facts {
		out = append(out, f.Title)
	}
	return out
}

// sortedTitles is titles, sorted.
func (b lensFactsBody) sortedTitles() []string {
	out := b.titles()
	sort.Strings(out)
	return out
}

// lensFactTitles GETs url from r and returns the sorted titles of its 200 body.
func lensFactTitles(t *testing.T, r http.Handler, url string) []string {
	t.Helper()
	return decodeLensFacts(t, getLensFacts(t, r, url)).sortedTitles()
}

// A duplicate relative path across the write and a read mount collapses to a
// single row: the WRITE repo's copy, with a bare (unqualified) path — even when
// the write repo's name sorts AFTER the read mount's in binding order, and even
// when the read mount's copy is the newer one.
func TestLensFacts_DuplicatePathWriteWins(t *testing.T) {
	t.Parallel()                                   // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	m, r := newLensStatsRouter(t, "zulu", "alpha") // write=zulu sorts after read=alpha
	writeCosFact(t, m.Get("zulu"), "kb/x/1.md", "Write copy", 0.9, nil)
	awaitNextSecond()
	writeCosFact(t, m.Get("alpha"), "kb/x/1.md", "Read copy", 0.9, nil)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))

	require.Len(t, body.Facts, 1, "deduped; body=%+v", body)
	require.Equal(t, 1, body.Total)
	f := body.Facts[0]
	require.Equal(t, "kb/x/1.md", f.Path, "bare path")
	require.Equal(t, "Write copy", f.Title, "the WRITE repo's copy")
	require.Equal(t, "zulu", f.Source.Repo)
}

// A fact that lives only on a read mount is qualified with kb://<id12>/… and
// carries that mount's source identity.
func TestLensFacts_ReadMountQualifiedPath(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	writeCosFact(t, m.Get("beta"), "kb/y/2.md", "Read only", 0.9, func(f *fact.Fact) {
		f.Kind, f.Type = fact.Pragmatic, fact.Policy
	})
	beta := m.Get("beta")
	wantID := federate.ID12(beta.ID())

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))

	require.Len(t, body.Facts, 1, "body=%+v", body)
	f := body.Facts[0]
	require.Equal(t, "kb://"+wantID+"/kb/y/2.md", f.Path)
	require.Equal(t, "beta", f.Source.Repo)
	require.Equal(t, wantID, f.Source.ID)
	require.Equal(t, beta.AgentBranch(), f.Source.Branch)
	require.Equal(t, "policy", f.Type)
	require.Equal(t, "pragmatic", f.Kind)
}

// repo=<name> narrows the fan-out to the named mount(s); other mounts' facts do
// not appear, and the total counts only the selected mounts.
func TestLensFacts_RepoFilterNarrows(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	writeCosFact(t, m.Get("alpha"), "kb/a/1.md", "Write fact", 0.9, nil)
	writeCosFact(t, m.Get("beta"), "kb/b/2.md", "Read fact", 0.9, nil)
	writeCosFact(t, m.Get("beta"), "kb/b/3.md", "Another read fact", 0.9, nil)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?repo=beta"))

	require.Equal(t, []string{"Another read fact", "Read fact"}, body.sortedTitles())
	require.Equal(t, 2, body.Total, "the count answers for the CURRENT query")
	for _, f := range body.Facts {
		require.Equal(t, "beta", f.Source.Repo)
	}
}

// An unknown repo= name is a well-formed request naming a nonexistent mount → 422.
func TestLensFacts_UnknownRepoFilter422(t *testing.T) {
	_, r := newLensStatsRouter(t, "alpha", "beta")

	rec := getLensFacts(t, r, "/lenses/eng/facts?repo=ghost")

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
}

// Recency ordering is by committed_at ACROSS mounts, not grouped by mount, and
// an ordinary truncated fan-out (each mount holds more than the page asked for)
// still answers in ONE round: deepening is driven by the merge horizon, not by
// truncation alone, or every page of every lens pays for the fork case that
// never happens to it.
func TestLensFacts_RecencyOrderingAcrossMounts(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	m, r := newLensStatsRouter(t, "alpha", "beta")
	// Interleaved commits: alpha, beta, alpha, beta (oldest first).
	for i, step := range []struct{ repo, title string }{
		{"alpha", "a-100"}, {"beta", "b-200"}, {"alpha", "a-300"}, {"beta", "b-400"},
	} {
		if i > 0 {
			awaitNextSecond()
		}
		writeCosFact(t, m.Get(step.repo), "kb/"+step.title+".md", step.title, 0.9, nil)
	}
	reads := countMountReads(t, m, []string{"alpha", "beta"}, "RecentFacts")

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=100"))
	require.Equal(t, []string{"b-400", "a-300", "b-200", "a-100"}, body.titles(),
		"interleaved by timestamp, not mount-grouped")

	for _, n := range []string{"alpha", "beta"} {
		reads[n]["RecentFacts"].Store(0)
	}
	page := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=1"))
	require.Equal(t, []string{"b-400"}, page.titles())
	require.Equal(t, 4, page.Total)
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 1, reads[n]["RecentFacts"].Load(),
			"mount %s: an ordinary truncated fan-out must not deepen", n)
	}
}

// seedFilterCorpus writes, into mount n, one fact per slug of filterSlugs: a
// plain `base` and a decoy for every filter, each differing from `base` in
// exactly the dimension that filter selects on.
func seedFilterCorpus(t *testing.T, ri *repos.RepoInstance, n string) {
	t.Helper()
	p := func(slug string) string { return "kb/" + n + "/" + slug + ".md" }
	spec := func(slug string, cos float64, mod func(f *fact.Fact)) factSpec {
		return factSpec{path: p(slug), title: n + " " + slug, mod: mod, cos: cos}
	}
	writeFactsInOneCommit(t, ri,
		spec("base", 0.9, nil),
		spec("lowsim", 0.6, nil),
		spec("unrelated", 0.9, func(f *fact.Fact) { f.Body = "nothing the query is about" }),
		spec("weak", 0.9, func(f *fact.Fact) { f.Confidence = 0.3 }),
		spec("store-only", 0.9, func(f *fact.Fact) { f.Domain = []string{"store"} }),
		spec("store-mcp", 0.9, func(f *fact.Fact) { f.Domain = []string{"store", "mcp"} }),
		spec("store-extra-mcp", 0.9, func(f *fact.Fact) { f.Domain = []string{"store-extra", "mcp"} }),
		spec("foo", 0.9, func(f *fact.Fact) { f.Entities = []string{"Foo"} }),
		spec("foo-bar", 0.9, func(f *fact.Fact) { f.Entities = []string{"Foo", "Bar"} }),
		spec("hypothesis", 0.9, func(f *fact.Fact) { f.Type = fact.Hypothesis }),
		spec("distilled", 0.9, func(f *fact.Fact) {
			f.Type = fact.Synthesis
			f.Refs = []string{p("base")}
		}),
		spec("discovered", 0.9, func(f *fact.Fact) {
			f.Type = fact.Hypothesis
			f.Origin = fact.Discovered
		}),
		spec("pragmatic", 0.9, func(f *fact.Fact) {
			f.Kind, f.Type = fact.Pragmatic, fact.Policy
		}))
	writeFactOp(t, ri, p("updated"), n+" updated", 0.9, "update", nil)
}

var filterSlugs = []string{
	"base", "lowsim", "unrelated", "weak", "store-only", "store-mcp", "store-extra-mcp", "foo", "foo-bar",
	"hypothesis", "distilled", "discovered", "pragmatic", "updated",
}

// titlesOf is the sorted titles of the given slugs on every mount.
func titlesOf(mounts []string, slugs ...string) []string {
	var out []string
	for _, m := range mounts {
		for _, s := range slugs {
			out = append(out, m+" "+s)
		}
	}
	sort.Strings(out)
	return out
}

// allSlugsBut is filterSlugs without the named ones.
func allSlugsBut(drop ...string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	var out []string
	for _, s := range filterSlugs {
		if !skip[s] {
			out = append(out, s)
		}
	}
	return out
}

// Every selecting filter (not just path/text) is forwarded to each mount, so a
// lens answers a filtered browse exactly like the repos it federates.
// Regression for the bug where the handler dropped all filters except path and
// text. Both mounts hold the same corpus (a base fact plus one decoy per filter,
// see seedFilterCorpus), so a filter that is dropped, or reaches only one mount,
// lets that mount's decoy into the union.
func TestLensFacts_ForwardsFullFilterSet(t *testing.T) {
	env := searchLens(t, storeProviders{}, "alpha", "beta")
	mounts := []string{"alpha", "beta"}
	for _, n := range mounts {
		seedFilterCorpus(t, env.m.Get(n), n)
	}
	const all = "/lenses/eng/facts?limit=100"

	for _, tc := range []struct {
		name  string
		query string
		want  []string // slugs, on both mounts
	}{
		{"no filter (control: every fact is listed)", "", filterSlugs},
		{"domain tokens", "&domain=store,mcp", []string{"store-mcp", "store-extra-mcp"}},
		{"domain_exact", "&domain=store,mcp&domain_exact=true", []string{"store-mcp"}},
		{"entities", "&entities=Foo,Bar", []string{"foo-bar"}},
		{"entity singular", "&entity=Foo", []string{"foo", "foo-bar"}},
		{"entity singular and plural merge", "&entity=Foo&entities=Bar", []string{"foo-bar"}},
		{"entities plural alone", "&entities=Foo", []string{"foo", "foo-bar"}},
		{"type and exclude_type", "&type=observation,hypothesis&exclude_type=hypothesis",
			allSlugsBut("hypothesis", "distilled", "discovered", "pragmatic")},
		{"kind", "&kind=pragmatic", []string{"pragmatic"}},
		{"exclude_kind", "&exclude_kind=pragmatic", allSlugsBut("pragmatic")},
		{"origin authored", "&origin=authored", allSlugsBut("distilled", "discovered")},
		{"origin list", "&origin=distilled,discovered", []string{"distilled", "discovered"}},
		{"ep learn", "&ep=learn", allSlugsBut("updated")},
		{"ep update", "&ep=update", []string{"updated"}},
		{"min_confidence", "&min_confidence=0.4", allSlugsBut("weak")},
		{"text", "&q=needle", allSlugsBut("unrelated")},
		{"text under the query alias", "&query=needle", allSlugsBut("unrelated")},
		{"text and min_similarity", "&q=needle&min_similarity=0.7", allSlugsBut("unrelated", "lowsim")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, titlesOf(mounts, tc.want...), lensFactTitles(t, env.r, all+tc.query))
		})
	}
}

// `topic` is shorthand for an ontology-root subdirectory filter: `?topic=X`
// rewrites to `path=kb/X/`, and an explicit `?path=` always wins. Mirrors the
// repo facts collection. Regression for the bug where the lens handler dropped
// `topic`, so a topic-scoped browse returned the full unscoped union. The path
// applies per mount, in that mount's own (repo-relative) terms.
func TestLensFacts_TopicShorthandRewritesPath(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	for _, n := range []string{"alpha", "beta"} {
		writeCosFact(t, m.Get(n), "kb/technology/"+n+".md", n+" technology", 0.9, nil)
		writeCosFact(t, m.Get(n), "kb/science/"+n+".md", n+" science", 0.9, nil)
		writeCosFact(t, m.Get(n), "kb/other/technology/"+n+".md", n+" misfiled", 0.9, nil)
	}

	require.Equal(t, []string{"alpha technology", "beta technology"},
		lensFactTitles(t, r, "/lenses/eng/facts?topic=technology"))
	// An explicit ?path= overrides ?topic= (both topics live in the preset, so
	// the ontology-aware skip does not confound the override check).
	require.Equal(t, []string{"alpha science", "beta science"},
		lensFactTitles(t, r, "/lenses/eng/facts?topic=technology&path=kb/science/"))
	// An unqualified path applies per mount.
	require.Equal(t, []string{"alpha science", "beta science"},
		lensFactTitles(t, r, "/lenses/eng/facts?path=kb/science/"))
	// A kb://<id12>/-qualified path names ONE mount and is applied to it
	// repo-relative.
	qualified := federate.QualifyPath(federate.ID12(m.Get("beta").ID()), "kb/science/")
	require.Equal(t, []string{"beta science"}, lensFactTitles(t, r, "/lenses/eng/facts?path="+qualified))
}

// A malformed numeric filter is a 400, matching the repo/lens search handlers,
// rather than being silently coerced to zero.
func TestLensFacts_InvalidNumericFilters400(t *testing.T) {
	_, r := newLensStatsRouter(t, "alpha")

	for _, q := range []string{"min_confidence=notanumber", "min_similarity=huh"} {
		rec := getLensFacts(t, r, "/lenses/eng/facts?"+q)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", q, rec.Body.String())
	}
}

// A text query orders the union by relevance (per-mount rank fused with RRF),
// NOT by committed_at. Lens-of-one identity: with a single mount, RRF's N=1
// identity preserves the mount's relevance order byte-for-byte, matching the
// repo /facts endpoint. Regression for the bug where every text query was
// re-sorted by recency via MergeRecent, burying the best match. The commits
// are written best-first, so recency order is the exact reverse of relevance.
func TestLensFacts_TextQueryPreservesRelevanceOrder(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	env := searchLens(t, storeProviders{}, "solo")
	ri := env.m.Get("solo")
	writeCosFact(t, ri, "kb/x/best.md", "best", 0.9, nil)
	awaitNextSecond()
	writeCosFact(t, ri, "kb/x/mid.md", "mid", 0.7, nil)
	awaitNextSecond()
	writeCosFact(t, ri, "kb/x/worst.md", "worst", 0.5, nil)

	body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?query=needle"))

	require.Equal(t, []string{"best", "mid", "worst"}, body.titles(), "relevance order, NOT recency")
	require.Greater(t, body.Facts[0].Score, body.Facts[2].Score)
	// The same corpus without a text query is the recency order.
	require.Equal(t, []string{"worst", "mid", "best"}, decodeLensFacts(t, env.get(t, "/lenses/eng/facts")).titles())
}

// Across mounts, a text query fuses by per-mount RANK (RRF), interleaving
// rank-0 rows first regardless of their timestamps — not a global recency sort.
func TestLensFacts_TextQueryFusesByRankNotTimestamp(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	env := searchLens(t, storeProviders{}, "alpha", "beta")
	writeFactsInOneCommit(t, env.m.Get("beta"),
		factSpec{path: "kb/b/0.md", title: "b0", cos: 0.8},
		factSpec{path: "kb/b/1.md", title: "b1", cos: 0.6})
	awaitNextSecond()
	// alpha's rows are all NEWER than beta's: a recency sort would put both
	// of them first.
	writeFactsInOneCommit(t, env.m.Get("alpha"),
		factSpec{path: "kb/a/0.md", title: "a0", cos: 0.9},
		factSpec{path: "kb/a/1.md", title: "a1", cos: 0.7})

	body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?query=needle"))

	// RRF: rank-0 rows first (a0, b0 — tie broken by mount order), then rank-1
	// (a1, b1). A recency sort would have produced a0,a1,b0,b1.
	require.Equal(t, []string{"a0", "b0", "a1", "b1"}, body.titles())
}

// A text query reaches past the first page: the retrieval depth covers
// offset+limit, so a page deep in the relevance list is served, not cut off.
func TestLensFacts_TextQueryReachesPastTheFirstPage(t *testing.T) {
	env := searchLens(t, storeProviders{}, "alpha")
	var specs []factSpec
	for i := 0; i < 12; i++ {
		specs = append(specs, factSpec{path: fmt.Sprintf("kb/a/%02d.md", i), title: fmt.Sprintf("a%02d", i)})
	}
	writeFactsInOneCommit(t, env.m.Get("alpha"), specs...)

	body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?query=needle&limit=5&offset=8"))

	require.Len(t, body.Facts, 4, "rows 8..11 of 12")
	require.Equal(t, 12, body.Total)
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensFacts_UnknownLens404(t *testing.T) {
	_, r := newLensStatsRouter(t, "alpha")

	rec := getLensFacts(t, r, "/lenses/missing/facts")

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// An empty union (no mount returns facts) yields facts:[] and total:0, never null.
func TestLensFacts_EmptyMounts(t *testing.T) {
	_, r := newLensStatsRouter(t, "alpha", "beta")

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))

	require.Equal(t, 0, body.Total)
	require.NotNil(t, body.Facts, "facts must be [] not null")
}

// ── Counts are not page sizes ────────────────────────────────────────────────
//
// The union total used to be len(merged): the size of the materialised
// candidate set, not the number of facts. With a fixed 500-row per-mount fetch
// a lens over a 1403-fact mount reported 500, and the dashboard — which sums
// each mount's real total — reported 1403 for the same corpus on the same
// screen. Two numbers, one truth, and the smaller one was in the browser.
//
// The store has always answered both questions separately: RecentFacts runs its
// own SELECT COUNT(*) and returns it alongside the page. The repo endpoint uses
// it. The lens handler discarded it with `entries, _, err`.

func TestLensFacts_TotalIsTheCorpusCountNotThePage(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	var alpha, beta []factSpec
	for i := 0; i < 7; i++ {
		alpha = append(alpha, factSpec{path: fmt.Sprintf("kb/a/%d.md", i), title: fmt.Sprintf("a%d", i)})
	}
	for i := 0; i < 4; i++ {
		beta = append(beta, factSpec{path: fmt.Sprintf("kb/b/%d.md", i), title: fmt.Sprintf("b%d", i)})
	}
	writeFactsInOneCommit(t, m.Get("alpha"), alpha...)
	writeFactsInOneCommit(t, m.Get("beta"), beta...)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=1"))

	require.Equal(t, 11, body.Total, "7+4 — the count must not be the page size")
	require.Len(t, body.Facts, 1, "limit still bounds the transfer")
	// The count has to answer for the CURRENT query, or narrowing sources would
	// leave a total describing a set the reader is no longer looking at.
	require.Equal(t, 4, decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=1&repo=beta")).Total)
}

// ── Depth follows the request, so nothing is out of reach ────────────────────
//
// The fixed 500 was a WINDOW: each mount handed over its 500 most recent and
// paging walked inside that set, so a mount's 501st-newest fact could not be
// reached at any offset. Commit timestamps are comparable across mounts, so
// there is no reason for a window — a row in the global page [offset, offset+
// limit) is always within its own mount's first offset+limit rows, which makes
// that the exact depth to ask each mount for.
//
// The depth is proved by reach, not by capturing the Limit the handler sends:
// the store answers only as many rows as it was asked for, so a page past the
// depth would come back short.

func TestLensFacts_ReachesPastTheOldCandidateCap(t *testing.T) {
	// The regression in one shot: a mount with more than 500 facts, read at an
	// offset beyond 500. Under the fixed window this returned nothing. All the
	// facts share one commit, so they list by path.
	m, r := newLensStatsRouter(t, "alpha")
	specs := make([]factSpec, 0, 520)
	for i := 0; i < 520; i++ {
		specs = append(specs, factSpec{path: fmt.Sprintf("kb/a/%03d.md", i), title: fmt.Sprintf("a%03d", i)})
	}
	writeFactsInOneCommit(t, m.Get("alpha"), specs...)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=5&offset=510"))

	require.Equal(t, []string{"a510", "a511", "a512", "a513", "a514"}, body.titles(),
		"rows past the old cap: offset 510 was unreachable before")
	require.Equal(t, 520, body.Total)

	// An absurd offset is an empty page with the true total — the depth is
	// clamped to a backstop, not wrapped into a negative LIMIT (SQLite's
	// spelling of no limit). That the clamp is exactly 10000 cannot be observed
	// without 10001 facts (indexing 10,010 facts measured 96 s); the constant
	// and the overflow arithmetic are pinned by TestLensFanoutDepth_*.
	empty := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=10&offset=99000000"))
	require.Empty(t, empty.Facts)
	require.NotNil(t, empty.Facts, "facts must be [] not null")
	require.Equal(t, 520, empty.Total)
}

// The count is EXACT when the fan-out held every row: a path on two mounts is
// one fact, and dedup can prove it. Summing per-mount COUNT(*) unconditionally
// would report two — which is why the sum is the fallback, not the rule.
func TestLensFacts_ExactUnionCountWhenNothingWasTruncated(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	writeCosFact(t, m.Get("alpha"), "kb/shared.md", "from-write", 0.9, nil)
	writeCosFact(t, m.Get("beta"), "kb/shared.md", "from-read", 0.9, nil)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))

	require.Equal(t, 1, body.Total, "one path is one fact when dedup can see both copies")
}

// The loop must not cost a second round in the ordinary case: if every mount
// handed over everything it had, there is no deeper row to go get. The counter
// sees one RecentFacts read per mount.
func TestLensFacts_NoRefanWhenNothingWasTruncated(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha", "beta")
	writeCosFact(t, m.Get("alpha"), "kb/a.md", "a", 0.9, nil)
	writeCosFact(t, m.Get("beta"), "kb/b.md", "b", 0.9, nil)
	reads := countMountReads(t, m, []string{"alpha", "beta"}, "RecentFacts")

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=50"))

	require.Equal(t, []string{"a", "b"}, body.sortedTitles())
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 1, reads[n]["RecentFacts"].Load(), "mount %s: a complete fan-out must not deepen", n)
	}
}

// ── Dedupe spends depth, so depth has to answer for it ───────────────────────
//
// lensFanoutDepth's proof holds for the PRE-dedupe union: a row in the global
// page sits within its own mount's first offset+limit rows. A duplicate that
// loses to the write mount still consumed a row of its mount's depth, so a
// re-rooted fork whose newest rows are all copies of the upstream's can spend
// the entire budget and contribute nothing of its own.
//
// The damage is not only a short page. Page 1 came back FULL — of the
// upstream's older facts — while the genuinely newest rows in the union were
// never fetched at all.

// A fork beside its upstream: `beta` is a fork of `alpha` that re-committed all
// three shared facts recently (so its copies are its newest rows and all lose
// the dedupe), and holds two unique facts that are older than those copies but
// far newer than anything on the write mount. Three commits, oldest first:
// alpha's three, beta's two own, beta's three copies.
func TestLensFacts_DedupedDuplicatesDoNotEatTheDepth(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	env := searchLens(t, storeProviders{}, "alpha", "beta")
	writeFactsInOneCommit(t, env.m.Get("alpha"),
		factSpec{path: "kb/a.md", title: "a"}, factSpec{path: "kb/b.md", title: "b"}, factSpec{path: "kb/c.md", title: "c"})
	awaitNextSecond()
	writeFactsInOneCommit(t, env.m.Get("beta"),
		factSpec{path: "kb/u1.md", title: "u1"}, factSpec{path: "kb/u2.md", title: "u2"})
	awaitNextSecond()
	writeFactsInOneCommit(t, env.m.Get("beta"),
		factSpec{path: "kb/a.md", title: "fork-a"}, factSpec{path: "kb/b.md", title: "fork-b"}, factSpec{path: "kb/c.md", title: "fork-c"})
	reads := countMountReads(t, env.m, []string{"alpha", "beta"}, "RecentFacts")

	t.Run("page one is the fork's own facts", func(t *testing.T) {
		// Depth starts at offset+limit = 2, which beta spends entirely on copies
		// that lose the dedupe. Its unique facts are the two newest rows in the
		// union and must still be what page 1 is — and getting there took a
		// deeper second round (the counter is the positive control that the
		// corpus really forces one).
		before := reads["beta"]["RecentFacts"].Load()
		body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?limit=2"))
		require.Equal(t, []string{"u1", "u2"}, body.titles(), "the fork's own facts are the newest in the union")
		require.Greater(t, reads["beta"]["RecentFacts"].Load()-before, int64(1), "the fan-out deepened")
	})

	t.Run("every row is reachable", func(t *testing.T) {
		// The reachability half of the same failure: with the depth spent on
		// duplicates, u1/u2 were unreachable at EVERY offset. Five distinct
		// facts: three shared (write copy wins) and the fork's two.
		seen := map[string]bool{}
		for off := 0; off < 6; off += 2 {
			body := decodeLensFacts(t, env.get(t, fmt.Sprintf("/lenses/eng/facts?limit=2&offset=%d", off)))
			for _, f := range body.Facts {
				seen[f.Title] = true
			}
		}
		require.Equal(t, map[string]bool{"a": true, "b": true, "c": true, "u1": true, "u2": true}, seen)
	})

	t.Run("a text query never re-fans", func(t *testing.T) {
		// Relevance ranks are not comparable across mounts, so there is no
		// timestamp horizon to satisfy and no deeper row to walk toward: one
		// round per mount, even on the corpus that forces the recency path to
		// deepen.
		before := reads["beta"]["RecentFacts"].Load()
		body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?query=widgets&limit=2"))
		require.Len(t, body.Facts, 2)
		require.EqualValues(t, 1, reads["beta"]["RecentFacts"].Load()-before)
	})
}
