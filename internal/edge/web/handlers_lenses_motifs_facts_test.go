package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/vocab"
)

// The lens motif carriers and the lens motif pivot (GET /lenses/{lens}/facts
// ?motifs=…) read REAL stores: only the lens-wide VOCABULARY is still answered
// by a stub (lensMotifsStub, until the motifs provider goes). A mount matches
// the spellings it is handed against the facts it holds, so which mount answers
// is decided by what the lens widens the query to.

// lensMotifsEnv is lens "eng" (write alpha, read beta) over real stores, whose
// vocabulary is answered by stub.
func lensMotifsEnv(t *testing.T, stub *lensMotifsStub) searchLensEnv {
	t.Helper()
	return searchLens(t, storeProviders{motifs: stub}, "alpha", "beta")
}

func TestLensMotifCluster_PivotLinkCarriesTheMergedMembers(t *testing.T) {
	env := lensMotifsEnv(t, splitVocabularyStub())
	// degradation-quiet merges the spellings quiet-degradation (beta's) and
	// silent-fallback (alpha's). The plural spelling and the strangers are out.
	writeCosFact(t, env.m.Get("alpha"), "kb/a1.md", "alpha silent", 0.9, withMotifs("silent-fallback"))
	writeCosFact(t, env.m.Get("alpha"), "kb/a2.md", "alpha plural", 0.9, withMotifs("silent-fallbacks"))
	writeCosFact(t, env.m.Get("alpha"), "kb/a3.md", "alpha stranger", 0.9, withMotifs("scope-creep"))
	writeCosFact(t, env.m.Get("beta"), "kb/b1.md", "beta quiet", 0.9, withMotifs("quiet-degradation"))
	writeCosFact(t, env.m.Get("beta"), "kb/b2.md", "beta stranger", 0.9, withMotifs("scope-creep"))

	body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/degradation-quiet"))

	href := body.Links.Facts.Href
	require.Contains(t, href, "/lenses/eng/facts?", "the facts link must point at the lens")
	require.Contains(t, href, "motifs=quiet-degradation%2Csilent-fallback")
	require.Contains(t, href, "motif_match=exact")
	// Every mount was asked with the same union filter, at the exact tier: each
	// mount's own carrier is in, the plural spelling (stem tier only) and the
	// strangers are out.
	var carriers []string
	for _, c := range body.Carriers {
		carriers = append(carriers, c.Title)
	}
	sort.Strings(carriers)
	require.Equal(t, []string{"alpha silent", "beta quiet"}, carriers)
	require.Equal(t, 2, body.CarrierCount)
	// And the preview cannot disagree with the listing the link opens.
	linked := decodeLensFacts(t, env.get(t, strings.TrimPrefix(href, APIBase)))
	require.Equal(t, carriers, linked.sortedTitles())
}

// TestLensMotifCluster_CarriersCarryQualifiedPaths is the lens-motif twin of
// TestLensStats_HighlightsCarryQualifiedPaths: every path-bearing row a lens
// read emits must carry the canonical wire path — bare for the write mount,
// kb://<id12>/… for a read mount — or opening it 404s against the write repo.
func TestLensMotifCluster_CarriersCarryQualifiedPaths(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	env := lensMotifsEnv(t, splitVocabularyStub())
	writeCosFact(t, env.m.Get("beta"), "kb/from-read.md", "read", 0.9, withMotifs("configuration-drifts"))
	awaitNextSecond()
	writeCosFact(t, env.m.Get("alpha"), "kb/from-write.md", "write", 0.9, withMotifs("config-drift"))

	body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/drift-config"))

	byTitle := map[string]string{}
	for _, c := range body.Carriers {
		byTitle[c.Title] = c.Path
	}
	require.Equal(t, "kb/from-write.md", byTitle["write"], "write mount carrier: the bare path")
	require.Equal(t, federate.QualifyPath(federate.ID12(env.m.Get("beta").ID()), "kb/from-read.md"), byTitle["read"])
	// Recency order across mounts, newest first.
	require.Len(t, body.Carriers, 2)
	require.Equal(t, "write", body.Carriers[0].Title, "newest-first across mounts")
}

// Qualification happens AFTER the dedupe: a re-rooted fork mounted beside its
// upstream shares fact UUIDs, so the same repo-relative path arrives from two
// mounts and the WRITE mount's copy must win exactly once.
func TestLensMotifCluster_CarrierDedupeKeepsTheWriteCopy(t *testing.T) {
	t.Parallel() // waits on commit stamps (see awaitNextSecond); overlaps the wait with the other such tests
	env := lensMotifsEnv(t, splitVocabularyStub())
	writeCosFact(t, env.m.Get("alpha"), "kb/dup.md", "from write", 0.9, withMotifs("config-drift"))
	awaitNextSecond()
	writeCosFact(t, env.m.Get("beta"), "kb/dup.md", "from read", 0.9, withMotifs("config-drift"))

	body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/drift-config"))

	require.Len(t, body.Carriers, 1, "deduped; %+v", body.Carriers)
	require.Equal(t, "from write", body.Carriers[0].Title)
	require.Equal(t, "kb/dup.md", body.Carriers[0].Path, "the write mount's bare-path copy")
	require.Equal(t, 1, body.CarrierCount, "the union holds one fact")
}

// carrier_count counts MATCHES, never fetched rows: bounding the preview must
// never bound the count (kb/invariants/web/collections/count-vs-transfer).
func TestLensMotifCluster_CarrierCountIsNotBoundedByThePreview(t *testing.T) {
	env := lensMotifsEnv(t, splitVocabularyStub())
	writeFactsInOneCommit(t, env.m.Get("alpha"),
		plainFact("kb/a0.md", "a0", withMotifs("config-drift")),
		plainFact("kb/a1.md", "a1", withMotifs("config-drift")),
		plainFact("kb/a2.md", "a2", withMotifs("config-drift")))
	writeFactsInOneCommit(t, env.m.Get("beta"),
		plainFact("kb/b0.md", "b0", withMotifs("configuration-drifts")),
		plainFact("kb/b1.md", "b1", withMotifs("configuration-drifts")),
		// The same path as alpha's a0: one fact in the union, two matches in
		// the per-mount counts.
		plainFact("kb/a0.md", "beta copy of a0", withMotifs("configuration-drifts")))

	// Unbounded, every mount hands over everything, so the union is deduped
	// and the count is exact: five facts.
	all := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/drift-config"))
	require.Len(t, all.Carriers, 5)
	require.Equal(t, 5, all.CarrierCount)

	// Bounded to one row, each mount truncates, the overlap is unknowable
	// without fetching the rest, and the count falls back to the summed
	// per-mount match counts (3+3), a documented upper bound — never to the
	// one row the preview shows.
	body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/drift-config?limit=1"))
	require.Len(t, body.Carriers, 1, "the preview is bounded by the requested limit")
	require.Equal(t, 6, body.CarrierCount, "the summed per-mount match counts")
}

// writeLensDriftCarriers writes the carriers of the merged `drift-config`
// cluster (splitVocabularyStub), split across mounts by SPELLING: alpha's five
// carry `config-drift` (the elected canonical), beta's two carry
// `configuration-drifts` and nothing else, and beta holds one stranger.
// splitVocabularyStub says df 5 + 2, so the merged row promises 7.
func writeLensDriftCarriers(t *testing.T, m *repos.Manager) {
	t.Helper()
	var alpha []factSpec
	for _, n := range []string{"0", "1", "2", "3", "4"} {
		alpha = append(alpha, plainFact("kb/a"+n+".md", "alpha "+n, withMotifs("config-drift")))
	}
	writeFactsInOneCommit(t, m.Get("alpha"), alpha...)
	writeFactsInOneCommit(t, m.Get("beta"),
		plainFact("kb/b0.md", "beta 0", withMotifs("configuration-drifts")),
		plainFact("kb/b1.md", "beta 1", withMotifs("configuration-drifts")),
		plainFact("kb/b2.md", "beta stranger", withMotifs("scope-creep")))
}

// THE REGRESSION. A pivot minted from the merged vocabulary sends ONE name —
// the elected canonical — and the store's exact tier is per-branch canonical
// equality, so beta (which spells the shape `configuration-drifts`) used to
// contribute zero. The row said df 7 and the list under it held 5.
//
// The count and the list are the same query or the surface is lying, so the
// assertion is exactly that: the pivot's total equals the df beside the name.
func TestLensFacts_PivotOnAMergedCanonicalReachesEveryMount(t *testing.T) {
	env := lensMotifsEnv(t, splitVocabularyStub())
	writeLensDriftCarriers(t, env.m)

	// The df the vocabulary row promises.
	vocabulary := decodeMotifs(t, env.get(t, "/lenses/eng/motifs"))
	wantDF := 0
	var canonical string
	for _, e := range vocabulary.Embedded.Motifs {
		if e.ClusterKey == "drift-config" {
			wantDF, canonical = e.DF, e.Canonical
		}
	}
	require.Equal(t, 7, wantDF, "fixture")
	require.Equal(t, "config-drift", canonical, "fixture")

	// The list that name pivots to.
	body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?motifs="+canonical+"&motif_match=exact"))

	require.Equal(t, wantDF, body.Total, "the df the row beside it promised")
	require.Len(t, body.Facts, wantDF)
	// Specifically: the mount that spells it differently is IN the answer, and
	// the stranger is not.
	fromBeta := 0
	for _, f := range body.Facts {
		if f.Source.Repo == "beta" {
			fromBeta++
			require.NotEqual(t, "beta stranger", f.Title)
		}
	}
	require.Equal(t, 2, fromBeta, "beta carries the cluster under another spelling")
}

// The widening runs over the FULL read set, never the narrowed one. A term
// minted from the whole vocabulary has to keep naming the same shape when the
// list under it is narrowed — resolving it through a one-mount union would
// re-run the exact bug, since that mount is the one spelling it differently.
func TestLensFacts_PivotWideningIgnoresRepoNarrowing(t *testing.T) {
	env := lensMotifsEnv(t, splitVocabularyStub())
	writeLensDriftCarriers(t, env.m)

	body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?motifs=config-drift&motif_match=exact&repo=beta"))

	require.Equal(t, 2, body.Total, "the term still names the merged shape: beta's two")
	require.Equal(t, []string{"beta 0", "beta 1"}, body.sortedTitles())
}

// A term in no cluster is left exactly as the caller sent it: its own
// singleton, which is what the store does with it too — each mount answers for
// the literal spelling, and nothing else comes along.
func TestLensFacts_UnknownMotifTermIsPassedThroughUnchanged(t *testing.T) {
	env := lensMotifsEnv(t, splitVocabularyStub())
	writeLensDriftCarriers(t, env.m)
	writeCosFact(t, env.m.Get("alpha"), "kb/n.md", "alpha unknown", 0.9, withMotifs("nothing-like-this"))
	writeCosFact(t, env.m.Get("beta"), "kb/n.md", "beta unknown", 0.9, withMotifs("nothing-like-this"))

	body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?motifs=nothing-like-this"))

	// Same repo-relative path on both mounts: deduped to the write mount's copy.
	require.Equal(t, []string{"alpha unknown"}, body.sortedTitles())
	// A spelling only beta carries is found on beta alone.
	writeCosFact(t, env.m.Get("beta"), "kb/o.md", "beta only", 0.9, withMotifs("only-on-beta"))
	require.Equal(t, []string{"beta only"}, lensFactTitles(t, env.r, "/lenses/eng/facts?motifs=only-on-beta"))
}

// A lens over ONE repo must send exactly what a repo request sends.
//
// NOT because the widening would be an identity there — it would not. Within
// one branch ClustersUnder groups mechanically-equal spellings into one cluster
// while the store's exact tier resolves a bare term through PER-SPELLING
// canonicals, so a widened single-mount term would reach a cluster sibling the
// same repo read directly does not. The skip is what keeps a lens of one
// answering exactly like the repo it wraps. The vocabulary here HAS the
// sibling, and the corpus has a fact carrying it, so a widened term would
// return that fact and the lens would differ from the repo.
func TestLensFacts_WideningIsAnIdentityForALensOfOne(t *testing.T) {
	env := searchLens(t, storeProviders{motifs: &lensMotifsStub{clusters: map[string][]vocab.MotifCluster{
		"alpha": {{ClusterKey: "drift-config", CanonicalID: "config-drift",
			Members: []string{"config-drift", "configuration-drifts"}, DF: 2, DFTotal: 2}},
	}}}, "alpha")
	writeCosFact(t, env.m.Get("alpha"), "kb/a.md", "carries the canonical", 0.9, withMotifs("config-drift"))
	writeCosFact(t, env.m.Get("alpha"), "kb/b.md", "carries the sibling", 0.9, withMotifs("configuration-drifts"))

	repo := factsWire{}
	require.NoError(t, json.Unmarshal(repoWire(t, env.r, "alpha", "facts?motifs=config-drift&motif_match=exact"), &repo))
	var repoTitles []string
	for _, f := range repo.Embedded.Facts {
		repoTitles = append(repoTitles, f.Title)
	}

	lens := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?motifs=config-drift&motif_match=exact"))

	require.Equal(t, []string{"carries the canonical"}, repoTitles, "the repo read: the bare term")
	require.Equal(t, repoTitles, lens.titles(), "a lens of one answers exactly like the repo it wraps")
	require.Equal(t, 1, lens.Total)
}

// A judge merge on ANY mount binds the whole lens, for reads.
//
// The pre-lens rule was that beta's identically-spelled fact must NOT answer,
// because beta never merged the spellings. That is what "resolution is per-mount"
// means when there is no lens-wide vocabulary to resolve against. There is one
// now, and it says these two spellings are one shape, so every mount answers
// for it. A lens answers exactly like a single repo
// (kb/decisions/lens/no-federation-metadata-in-responses), and inside a single
// repo one judge merge binds every fact in it. Keeping the old rule would mean a
// lens whose answer depends on which mount a reader's vocabulary happened to
// come from — the shape the merged vocabulary exists to remove.
func TestLensFacts_AMergeOnAnyMountBindsTheWholeLens(t *testing.T) {
	env := lensMotifsEnv(t, divergentVocabulariesUnion())
	seedDivergentVocabularies(t, env.m)

	titles := lensFactTitles(t, env.r, "/lenses/eng/facts?motifs=config-drift")

	// alpha merged the spellings, so its aliased fact answers; beta never did and
	// answers anyway, because the LENS did; and beta's exact carrier is still
	// there; and a fact outside the shape does not answer.
	require.Equal(t, []string{"Alpha aliased", "Beta aliased", "Beta exact"}, titles)
}

// seedDivergentVocabularies writes the asymmetry the semantics turn on: mount
// alpha judge-merged "config-drift" and "configuration-drifts" (its vocabulary,
// divergentVocabulariesUnion); mount beta never did, and holds both spellings.
func seedDivergentVocabularies(t *testing.T, m *repos.Manager) {
	t.Helper()
	writeCosFact(t, m.Get("alpha"), "kb/alpha-aliased.md", "Alpha aliased", 0.9, withMotifs("configuration-drifts"))
	writeCosFact(t, m.Get("beta"), "kb/beta-exact.md", "Beta exact", 0.9, withMotifs("config-drift"))
	writeCosFact(t, m.Get("beta"), "kb/beta-aliased.md", "Beta aliased", 0.9, withMotifs("configuration-drifts"))
	writeCosFact(t, m.Get("beta"), "kb/beta-other.md", "Beta other", 0.9, withMotifs("something-else"))
}

// The merge binds READS through the lens; it rewrites nothing. Beta's own
// vocabulary is untouched, so beta read directly still resolves only its exact
// spelling — the per-mount rule the test above used to assert is still the rule
// for a mount read on its own.
func TestLensFacts_TheMergeBindsReadsAndRewritesNothing(t *testing.T) {
	env := lensMotifsEnv(t, divergentVocabulariesUnion())
	seedDivergentVocabularies(t, env.m)

	// The lens widened the TERM, which is why "Beta aliased" comes back.
	require.Contains(t, lensFactTitles(t, env.r, "/lenses/eng/facts?motifs=config-drift"), "Beta aliased")

	// It did not touch beta's alias table: beta read directly, after the lens
	// request, still answers only for the exact spelling.
	var direct factsWire
	require.NoError(t, json.Unmarshal(repoWire(t, env.r, "beta", "facts?motifs=config-drift&motif_match=exact"), &direct))
	var got []string
	for _, f := range direct.Embedded.Facts {
		got = append(got, f.Title)
	}
	require.Equal(t, []string{"Beta exact"}, got)
}

// Motif filtering fans out to EVERY mount, carrying the same terms and tier —
// the per-mount-resolution semantics made testable. Each mount expands the
// terms against its own alias vocabulary; the handler's job is to make sure
// every mount is asked the same question. The corpus separates the exact tier
// from the token-2 tier on each mount (a fact sharing two stemmed tokens with
// the query but not its spelling), so a mount that was not handed the tier
// answers differently from one that was.
func TestLensFacts_MotifFilterReachesEveryMount(t *testing.T) {
	env := searchLens(t, storeProviders{}, "alpha", "beta")
	writeCosFact(t, env.m.Get("alpha"), "kb/a1.md", "alpha exact", 0.9, withMotifs("silent-fallback-path"))
	writeCosFact(t, env.m.Get("alpha"), "kb/a2.md", "alpha near", 0.9, withMotifs("silent-fallback-retry"))
	writeCosFact(t, env.m.Get("beta"), "kb/b1.md", "beta near", 0.9, withMotifs("silent-fallback-timeout"))
	writeCosFact(t, env.m.Get("beta"), "kb/b2.md", "beta unrelated", 0.9, withMotifs("scope-creep"))
	reads := countMountReads(t, env.m, []string{"alpha", "beta"}, "RecentFacts")

	require.Equal(t, []string{"alpha exact"},
		lensFactTitles(t, env.r, "/lenses/eng/facts?motifs=silent-fallback-path&motif_match=exact"))
	require.Equal(t, []string{"alpha exact", "alpha near", "beta near"},
		lensFactTitles(t, env.r, "/lenses/eng/facts?motifs=silent-fallback-path&motif_match=token-2"))
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 2, reads[n]["RecentFacts"].Load(), "mount %s must be read by both requests", n)
	}
}

// The tiers this surface refuses are refused before the fan-out starts — no
// mount is queried at all (C3/MN6). A permitted tier does read every mount, so
// the counter could have moved.
func TestLensFacts_LooseMotifTierRejected(t *testing.T) {
	env := searchLens(t, storeProviders{}, "alpha", "beta")
	reads := countMountReads(t, env.m, []string{"alpha", "beta"}, "RecentFacts")

	rec := env.get(t, "/lenses/eng/facts?motifs=a-b&motif_match=token-1")

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 0, reads[n]["RecentFacts"].Load(), "mount %s was queried despite the rejection", n)
	}
	rec = env.get(t, "/lenses/eng/facts?motifs=a-b&motif_match=token-2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 1, reads[n]["RecentFacts"].Load(), "mount %s", n)
	}
}

// The union rows carry each fact's motifs; a motif-free row omits the key.
func TestLensFacts_MotifsOnTheWire(t *testing.T) {
	m, r := newLensStatsRouter(t, "alpha")
	writeCosFact(t, m.Get("alpha"), "kb/a.md", "Carries a motif", 0.9, withMotifs("a-b"))
	writeCosFact(t, m.Get("alpha"), "kb/b.md", "Carries none", 0.9, nil)

	rec := getLensFacts(t, r, "/lenses/eng/facts")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Facts []map[string]any `json:"facts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.Len(t, body.Facts, 2)
	for _, f := range body.Facts {
		switch f["title"] {
		case "Carries a motif":
			require.Equal(t, []any{"a-b"}, f["motifs"])
		case "Carries none":
			_, present := f["motifs"]
			require.False(t, present, "motif-free row must omit the key entirely; body=%s", rec.Body.String())
		default:
			t.Fatalf("unexpected row %v", f["title"])
		}
	}
}

// The widening reads every mount, so a mount pinned to a deleted branch fails
// there — and the 404 it produces has to NAME that branch. writeStoreError
// renders model.ErrBranchNotFound as `no branch named "<branch>"`, so a
// fan-out that dropped the branch on its way out would print the message with
// the one word it exists to carry missing.
func TestLensFacts_MotifWideningNamesTheFailingMountsBranch(t *testing.T) {
	stub := splitVocabularyStub()
	stub.errRepo = "beta"
	stub.errWith = model.ErrBranchNotFound
	env := lensMotifsEnv(t, stub)
	writeLensDriftCarriers(t, env.m)

	rec := env.get(t, "/lenses/eng/facts?motifs=config-drift&motif_match=exact")

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	// Decoded, not substring-matched: the detail carries the branch in quotes
	// and the body is JSON, so a literal-quote Contains check compares against
	// the wrong escaping and passes or fails for the wrong reason.
	var problem struct {
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem), rec.Body.String())
	require.Equal(t, `no branch named "`+env.m.Get("beta").AgentBranch()+`"`, problem.Detail,
		"the failing mount's branch, not an empty one")
}

// A mount failing the widening fails the WHOLE request, exactly as a mount
// failing the vocabulary read does — the term is resolved against the lens, and
// a lens does not answer from a shrunken read set (RFC §9.1). This is the same
// rule as everywhere else; it is asserted here because the widening is the one
// place it reaches a mount the caller did not ask about.
func TestLensFacts_MotifWideningFailsTheWholeRequest(t *testing.T) {
	stub := splitVocabularyStub()
	stub.errRepo = "beta"
	env := lensMotifsEnv(t, stub)
	writeLensDriftCarriers(t, env.m)

	// ...and it does so even when the caller narrowed beta AWAY: the term's
	// meaning is lens-wide, so beta is still read to resolve it. That coupling
	// is deliberate and documented; this pins that it is real rather than
	// accidental, so a later change cannot quietly drop it as a bug.
	rec := env.get(t, "/lenses/eng/facts?motifs=config-drift&repo=alpha")

	// The exact status, not merely "not 200": the stub fails with a plain
	// error, so 500 is the one right answer here.
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a mount excluded by repo= still resolves the term, so its failure fails the request; body=%s", rec.Body.String())
}
