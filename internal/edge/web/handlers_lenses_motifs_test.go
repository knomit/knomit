package web

import (
	"context"
	"encoding/json"
	"errors"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/vocab"
)

// The lens motif vocabulary reads every mount's REAL store: each mount's
// vocabulary is what its own facts, judge merges and definitions make it, and
// the lens merges them (federate.MotifUnion).

// writeLensVocabulary writes the two vocabularies the lens collection and
// detail tests merge, into lens "eng" (write alpha, read beta). Three kinds of
// overlap:
//
//   - `config-drift` is the SAME cluster key on both mounts. alpha carries the
//     spelling `config-drift` 4 times (canonical config-drift, df 4). beta
//     judge-merged `config-drift` (1) with `configuration-drifts` (2), so its
//     cluster keeps the key `config-drift` (the smaller of the two grouping
//     keys) under the canonical configuration-drifts, df 3. One row: df 7,
//     canonical config-drift (alpha's, 4 > 3).
//   - alpha judge-merged `quiet-degradation` (2) with `silent-fallback` (1): key
//     `degradation-quiet`, canonical quiet-degradation, df 3. beta carries
//     `silent-fallback` 5 times under ITS grouping key `fallback-silent`. The
//     keys differ, the spelling is shared, so it is still one row, keyed by the
//     smaller key degradation-quiet: df 8, canonical silent-fallback (beta's,
//     5 > 3).
//   - `creep-scope` (scope-creep) is alpha's only: df 1.
//
// Two carriers of config-drift on alpha and one on beta live under
// kb/technology/ (a topic of the preset ontology, so the lens fan-out does not
// skip it); everything else lives under kb/other/. Four of beta's five
// silent-fallback carriers are pragmatic.
//
// Health, over authored facts, per mount: alpha clusters 3, recurring 2, links
// 5 (3+2), epistemic recurring 2; beta clusters 2, recurring 2, links 6 (2+4),
// epistemic recurring 1. Pooled: clusters 5, recurring 4, mints 5, links 11,
// epistemic recurring 3.
func writeLensVocabulary(t *testing.T, m *repos.Manager) {
	t.Helper()
	alpha, beta := m.Get("alpha"), m.Get("beta")
	writeFactsInOneCommit(t, alpha,
		plainFact("kb/technology/a-cd0.md", "alpha drift 0", withMotifs("config-drift")),
		plainFact("kb/technology/a-cd1.md", "alpha drift 1", withMotifs("config-drift")),
		plainFact("kb/other/a-cd2.md", "alpha drift 2", withMotifs("config-drift")),
		plainFact("kb/other/a-cd3.md", "alpha drift 3", withMotifs("config-drift")),
		plainFact("kb/other/a-qd0.md", "alpha quiet 0", withMotifs("quiet-degradation")),
		plainFact("kb/other/a-qd1.md", "alpha quiet 1", withMotifs("quiet-degradation")),
		plainFact("kb/other/a-sf0.md", "alpha silent 0", withMotifs("silent-fallback")),
		plainFact("kb/other/a-sc.md", "alpha scope", withMotifs("scope-creep")))
	judgeMerge(t, alpha, "quiet-degradation", "silent-fallback", "one mechanism")

	writeFactsInOneCommit(t, beta,
		plainFact("kb/technology/b-cr0.md", "beta drifts 0", withMotifs("configuration-drifts")),
		plainFact("kb/other/b-cr1.md", "beta drifts 1", withMotifs("configuration-drifts")),
		plainFact("kb/other/b-cd0.md", "beta drift 0", withMotifs("config-drift")),
		plainFact("kb/other/b-sf0.md", "beta silent 0", withMotifs("silent-fallback")),
		plainFact("kb/other/b-sf1.md", "beta silent 1", withKindMotifs(true, "silent-fallback")),
		plainFact("kb/other/b-sf2.md", "beta silent 2", withKindMotifs(true, "silent-fallback")),
		plainFact("kb/other/b-sf3.md", "beta silent 3", withKindMotifs(true, "silent-fallback")),
		plainFact("kb/other/b-sf4.md", "beta silent 4", withKindMotifs(true, "silent-fallback")))
	judgeMerge(t, beta, "config-drift", "configuration-drifts", "same mechanism")
}

// lensVocabulary is lens "eng" (write alpha, read beta) over real stores
// holding writeLensVocabulary.
func lensVocabulary(t *testing.T) searchLensEnv {
	t.Helper()
	env := searchLens(t, "alpha", "beta")
	writeLensVocabulary(t, env.m)
	return env
}

// ─── collection ──────────────────────────────────────────────────────────────

func TestLensMotifs_UnionMergesClustersAcrossMounts(t *testing.T) {
	env := lensVocabulary(t)
	rec := env.get(t, "/lenses/eng/motifs")
	body := decodeMotifs(t, rec)

	require.Equal(t, 3, body.Count, "three merged clusters")
	// df-desc: the shared-spelling row 8, the shared-key row 7, creep-scope 1.
	require.Equal(t, []string{"degradation-quiet", "config-drift", "creep-scope"}, body.keys())

	// Same key on both mounts: members union, df and df_total sum, and the
	// canonical is the higher-df constituent's representative (alpha's 4 > 3).
	drift := body.entry(t, "config-drift")
	require.Equal(t, []string{"config-drift", "configuration-drifts"}, drift.Members, "the sorted union")
	require.Equal(t, [2]int{7, 7}, [2]int{drift.DF, drift.DFTotal}, "per-mount sums")
	require.Equal(t, "config-drift", drift.Canonical)

	// Different keys, shared spelling: still ONE row, keyed by the smallest
	// constituent key, and here the READ mount's representative wins (5 > 3).
	fallback := body.entry(t, "degradation-quiet")
	require.Equal(t, []string{"quiet-degradation", "silent-fallback"}, fallback.Members)
	require.Equal(t, 8, fallback.DF)
	require.Equal(t, "silent-fallback", fallback.Canonical)
	// The spelling must appear in exactly ONE row — two rows carrying it would
	// pivot to different lists from names the reader cannot tell apart.
	rows := 0
	for _, e := range body.Embedded.Motifs {
		for _, m := range e.Members {
			if m == "silent-fallback" {
				rows++
			}
		}
	}
	require.Equal(t, 1, rows, "silent-fallback must appear in exactly one row")

	require.Equal(t, 1, body.entry(t, "creep-scope").DF)
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
}

// Self links address the LENS, not a mount: a row's self link must be openable
// as /lenses/{lens}/motifs/{key}.
func TestLensMotifs_LinksAddressTheLens(t *testing.T) {
	env := lensVocabulary(t)
	body := decodeMotifs(t, env.get(t, "/lenses/eng/motifs?sort=name"))

	if !strings.HasSuffix(body.Links.Self.Href, "/lenses/eng/motifs?sort=name") {
		t.Errorf("collection self: got %q", body.Links.Self.Href)
	}
	require.Len(t, body.Embedded.Motifs, 3)
	for _, e := range body.Embedded.Motifs {
		want := "/lenses/eng/motifs/" + e.ClusterKey
		if !strings.HasSuffix(e.Links.Self.Href, want) {
			t.Errorf("entry self: got %q, want a href ending %q", e.Links.Self.Href, want)
		}
		decodeMotifDetail(t, env.get(t, strings.TrimPrefix(e.Links.Self.Href, httpx.APIBase)))
	}
}

// Health counts are POOLED across mounts and the two ratios are derived ONCE
// from the pooled totals — never a mean of per-mount ratios.
func TestLensMotifs_HealthPoolsCountsAndDerivesRatiosOnce(t *testing.T) {
	env := lensVocabulary(t)
	h := decodeMotifs(t, env.get(t, "/lenses/eng/motifs")).Health

	require.Equal(t, 5, h.AuthoredClusters)
	require.Equal(t, 4, h.AuthoredRecurring)
	require.Equal(t, 5, h.AuthoredMints)
	require.Equal(t, 11, h.AuthoredLinks)
	require.Equal(t, 3, h.AuthoredEpistemicRecurring)
	pooled := vocab.MotifVocabularyHealth{Clusters: 5, Recurring: 4, Mints: 5, Links: 11, EpistemicRecurring: 3}
	// Per mount the recurrence rates are 2/3 and 2/2 (mean 0.833); pooled 0.8.
	require.Equal(t, pooled.RecurrenceRate(), h.RecurrenceRate, "the rule applied ONCE to pooled totals")
	// Per mount the mint/link ratios are 3/5 and 2/6 (mean 0.467); pooled 5/11.
	// This is the assertion that fails if the handler averages ratios.
	require.Equal(t, pooled.MintToLinkRatio(), h.MintToLinkRatio, "pooled, not a mean of ratios")
}

// ?path= is SCOPE: it reaches every mount, and it narrows the health block as
// well as the list — exactly as it does on the repo endpoint. Under
// kb/technology/ only config-drift has carriers: alpha's two and beta's one.
func TestLensMotifs_PathScopesEveryMountAndItsHealth(t *testing.T) {
	env := lensVocabulary(t)
	body := decodeMotifs(t, env.get(t, "/lenses/eng/motifs?path=kb/technology/"))

	require.Equal(t, []string{"config-drift"}, body.keys())
	drift := body.entry(t, "config-drift")
	require.Equal(t, 3, drift.DF, "alpha's 2 + beta's 1: the scope reached both mounts")
	require.Equal(t, 7, drift.DFTotal, "df_total stays branch-wide")
	// alpha: one cluster, df 2; beta: one cluster, df 1.
	require.Equal(t, 2, body.Health.AuthoredClusters)
	require.Equal(t, 1, body.Health.AuthoredRecurring)
	require.Equal(t, 1, body.Health.AuthoredLinks)
}

// repo=<name> narrows the fan-out; an unknown name is a 422 (the shared lens
// union-read filter).
func TestLensMotifs_RepoFilterNarrows(t *testing.T) {
	env := lensVocabulary(t)
	body := decodeMotifs(t, env.get(t, "/lenses/eng/motifs?repo=beta"))
	// beta's own two clusters; alpha's creep-scope does not leak through, and
	// silent-fallback keeps beta's key now that alpha's merge is not read.
	require.Equal(t, []string{"fallback-silent", "config-drift"}, body.keys())
	require.Equal(t, 3, body.entry(t, "config-drift").DF, "beta's df alone")
}

func TestLensMotifs_UnknownRepoFilterIs422(t *testing.T) {
	env := lensVocabulary(t)
	rec := env.get(t, "/lenses/eng/motifs?repo=nope")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// A lens never silently shrinks its read set: one mount failing any of its
// vocabulary reads fails the WHOLE request (RFC §9.1), rather than returning
// the surviving mount's vocabulary as if it were the union's.
func TestLensMotifs_MountErrorFailsTheWholeRequest(t *testing.T) {
	for _, read := range []vocab.ReadName{vocab.ReadClustersUnder, vocab.ReadDefinitions, vocab.ReadVocabularyHealthUnder} {
		t.Run(string(read), func(t *testing.T) {
			env := lensVocabulary(t)
			sentinel := errors.New("beta " + string(read) + " on fire")
			var hits atomic.Int64
			failMotifRead(t, env.m.Get("beta"), read, sentinel, &hits)

			requireFailedWith(t, env.get(t, "/lenses/eng/motifs"), sentinel, &hits)
		})
	}
}

// ?q= narrows the list over member spellings AND definition text, and — unlike
// ?path= — leaves the health block alone.
func TestLensMotifs_QNarrowsListAcrossMountsButNotHealth(t *testing.T) {
	env := lensVocabulary(t)
	// The definition lives on the READ mount; a handler that only looks
	// definitions up on the write repo would miss this match.
	defineMotif(t, env.m.Get("beta"), "config-drift", "applied state diverges", false)

	body := decodeMotifs(t, env.get(t, "/lenses/eng/motifs?q=diverges"))
	require.Equal(t, []string{"config-drift"}, body.keys(), "q over definitions")
	require.Equal(t, 1, body.Count)
	require.Equal(t, "applied state diverges", body.Embedded.Motifs[0].Definition)
	require.Equal(t, 5, body.Health.AuthoredClusters, "health must ignore q")
}

// The definition shown for a merged cluster is the freshest one across mounts:
// a `current` definition beats a `stale` one wherever it lives — on the read
// mount, and on the write mount.
func TestLensMotifs_FreshestDefinitionWins(t *testing.T) {
	env := lensVocabulary(t)
	alpha, beta := env.m.Get("alpha"), env.m.Get("beta")
	shown := func() (string, string) {
		t.Helper()
		body := decodeMotifs(t, env.get(t, "/lenses/eng/motifs?q=wording"))
		require.Len(t, body.Embedded.Motifs, 1, "%+v", body.Embedded.Motifs)
		return body.Embedded.Motifs[0].Definition, body.Embedded.Motifs[0].DefinitionState
	}

	defineMotif(t, alpha, "config-drift", "stale alpha wording", true)
	defineMotif(t, beta, "config-drift", "current beta wording", false)
	def, state := shown()
	require.Equal(t, "current beta wording", def, "the current one from the read mount")
	require.Equal(t, "current", state)

	defineMotif(t, alpha, "config-drift", "current alpha wording", false)
	defineMotif(t, beta, "config-drift", "stale beta wording", true)
	def, state = shown()
	require.Equal(t, "current alpha wording", def, "the current one from the write mount")
	require.Equal(t, "current", state)

	// Equally fresh on both mounts: the tie goes to the WRITE mount. Asked
	// through two lenses over the same two stores, so the write mount is first
	// in binding order once (eng: write alpha) and last once (rev: write beta,
	// mounts are name-sorted): the winner must follow the write mount, not the
	// position.
	defineMotif(t, beta, "config-drift", "current beta wording", false)
	createLens(t, env.m, env.r, `{"name":"rev","write":"beta","reads":[{"repo":"alpha"}]}`)
	for lens, want := range map[string]string{"eng": "current alpha wording", "rev": "current beta wording"} {
		body := decodeMotifs(t, env.get(t, "/lenses/"+lens+"/motifs?q=wording"))
		require.Len(t, body.Embedded.Motifs, 1, "%s: %+v", lens, body.Embedded.Motifs)
		require.Equal(t, want, body.Embedded.Motifs[0].Definition, "%s: a tie goes to the write mount", lens)
		require.Equal(t, "current", body.Embedded.Motifs[0].DefinitionState, lens)
	}
}

// Paging is by cluster over the MERGED vocabulary, and `count` is the number of
// merged clusters the query matches — never the number of rows transferred.
func TestLensMotifs_PagingCountsMatchesNotRows(t *testing.T) {
	env := lensVocabulary(t)
	body := decodeMotifs(t, env.get(t, "/lenses/eng/motifs?limit=1"))
	require.Equal(t, 3, body.Count, "matches, not the 1 row fetched")
	require.Equal(t, []string{"degradation-quiet"}, body.keys())
	require.NotNil(t, body.Links.Next)
	require.Contains(t, body.Links.Next.Href, "offset=1")
}

func TestLensMotifs_LimitOverTheCeilingIs400(t *testing.T) {
	env := lensVocabulary(t)
	rec := env.get(t, "/lenses/eng/motifs?limit=99999")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (hard ceiling, never a quiet clamp)", rec.Code)
	}
}

// A lens over ONE repo answers with that repo's vocabulary unchanged — same
// keys, same canonicals, same members, same counts, same definitions, same
// health. The merge must be an identity there, so the lens answer is compared
// with the repo's own.
func TestLensMotifs_LensOfOneIsTheRepoVocabulary(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	writeLensVocabulary(t, env.m)
	alpha := env.m.Get("alpha")
	defineMotif(t, alpha, "config-drift", "Configured state diverges from applied state.", false)
	createLens(t, env.m, env.r, `{"name":"solo","write":"alpha","reads":[]}`)

	var repo motifsCollectionBody
	require.NoError(t, json.Unmarshal(repoWire(t, env.r, "alpha", "motifs"), &repo))
	lens := decodeMotifs(t, env.get(t, "/lenses/solo/motifs"))

	require.Equal(t, []string{"config-drift", "degradation-quiet", "creep-scope"}, repo.keys(), "fixture")
	require.Equal(t, repo.Count, lens.Count)
	require.Equal(t, repo.Health, lens.Health)
	for i := range repo.Embedded.Motifs {
		want, got := repo.Embedded.Motifs[i], lens.Embedded.Motifs[i]
		// The self links differ by surface, by design; each is checked against
		// its own base.
		require.True(t, strings.HasSuffix(got.Links.Self.Href, "/lenses/solo/motifs/"+want.ClusterKey), got.Links.Self.Href)
		want.Links, got.Links = motifsCollectionEntry{}.Links, motifsCollectionEntry{}.Links
		require.Equal(t, want, got, "row %d", i)
	}
}

// ─── cluster detail ──────────────────────────────────────────────────────────

func TestLensMotifCluster_ResolvesByMergedKeyConstituentKeyAndSpelling(t *testing.T) {
	env := lensVocabulary(t)

	// The merged key, the OTHER mount's constituent key, and each member
	// spelling all address the same merged cluster.
	for _, key := range []string{"degradation-quiet", "fallback-silent", "silent-fallback", "quiet-degradation"} {
		body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/"+key))
		require.Equal(t, "degradation-quiet", body.ClusterKey, "%q", key)
		require.Equal(t, []string{"quiet-degradation", "silent-fallback"}, body.Members, "%q", key)
		require.True(t, strings.HasSuffix(body.Links.Self.Href, "/lenses/eng/motifs/degradation-quiet"),
			"%q self link: got %q, want the merged key", key, body.Links.Self.Href)
	}
}

func TestLensMotifCluster_UnknownIs404(t *testing.T) {
	env := lensVocabulary(t)
	rec := env.get(t, "/lenses/eng/motifs/nothing-like-this")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// Alias rows merge across mounts, write mount first — the audit trail for a
// spelling comes from the mount that actually recorded a decision about it.
// `config-drift` has a row on both mounts: alpha's ("canonical", no merge
// there) wins over beta's ("judge"). `configuration-drifts` has a row on beta
// only, and keeps beta's audit trail.
func TestLensMotifCluster_AliasesMergeWriteFirst(t *testing.T) {
	env := lensVocabulary(t)
	body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/config-drift"))

	require.Equal(t, map[string]string{
		"config-drift":         "canonical/",
		"configuration-drifts": "judge/same mechanism",
	}, body.aliasesByMotif())
}

func TestLensMotifCluster_MountErrorFailsTheWholeRequest(t *testing.T) {
	for _, read := range []vocab.ReadName{vocab.ReadClustersUnder, vocab.ReadDefinitions, vocab.ReadAliasRows} {
		t.Run(string(read), func(t *testing.T) {
			env := lensVocabulary(t)
			sentinel := errors.New("beta " + string(read) + " on fire")
			var hits atomic.Int64
			failMotifRead(t, env.m.Get("beta"), read, sentinel, &hits)

			requireFailedWith(t, env.get(t, "/lenses/eng/motifs/config-drift"), sentinel, &hits)
		})
	}
}

// The detail is addressed by IDENTITY, so it is branch-wide on every mount: no
// path scope reaches the fan-out, exactly as the repo detail passes "". Scoped
// to kb/technology/ the cluster would count 3; branch-wide it counts 7.
func TestLensMotifCluster_IsBranchWideOnEveryMount(t *testing.T) {
	env := lensVocabulary(t)
	body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/config-drift?path=kb/technology/"))
	require.Equal(t, [2]int{7, 7}, [2]int{body.DF, body.DFTotal})
	require.Equal(t, 7, body.CarrierCount)
}

// ─── the pivot the vocabulary promises ───────────────────────────────────────

// motifSorted copies and sorts, so an assertion on the widened set does not
// depend on the union's iteration order.
func motifSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// /search takes the same widening — the pivot's ≈ segment and the filter chip
// both reach it, and a relevance query that dropped a mount would be the same
// defect with a different envelope. beta spells the shape `config-drifts`,
// which shares alpha's cluster key, so the lens merges the two; beta's own
// exact tier knows nothing of `config-drift` and resolves it only to itself.
func TestLensSearch_PivotOnAMergedCanonicalReachesEveryMount(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	writeCosFact(t, env.m.Get("alpha"), "kb/a0.md", "alpha 0", 0.9, withMotifs("config-drift"))
	writeCosFact(t, env.m.Get("beta"), "kb/b0.md", "beta 0", 0.8, withMotifs("config-drifts"))
	writeCosFact(t, env.m.Get("beta"), "kb/b1.md", "beta 1", 0.7, withMotifs("config-drifts"))
	seen := observeMountSearch(t, env.m, "alpha", "beta")

	rec := env.get(t, "/lenses/eng/search?q=drift&motifs=config-drift&motif_match=exact")
	body := decodeLensSearch(t, rec)

	// The mount that spells the shape differently is IN the answer.
	fromBeta := 0
	for _, res := range body.Results {
		if res.Source.Repo == "beta" {
			fromBeta++
		}
	}
	if fromBeta != 2 || len(body.Results) != 3 {
		t.Errorf("got %d rows (%d from beta), want 3 (2 from beta): %+v", len(body.Results), fromBeta, body.Results)
	}
	// Every mount was asked about the WHOLE shape, not about one mount's name.
	for _, name := range []string{"alpha", "beta"} {
		got := seen.of(name)
		require.Len(t, got, 1, name)
		if joined := strings.Join(motifSorted(got[0].Motifs), ","); joined != "config-drift,config-drifts" {
			t.Errorf("%s was asked for %q, want the merged member list", name, joined)
		}
	}
}

// A cluster key is a mechanical function of a spelling, so an alias row on one
// mount can name the SAME key as a merged cluster without that mount having
// contributed to it. Here beta once judge-merged `config-drifts` with
// `config-skew` (key config-drift, rationale "beta's own unrelated merge") and
// has since rewritten the only fact that carried them: the alias row survives
// (rows change only at a rebuild), but beta contributes nothing to the lens's
// config-drift cluster, which is alpha's alone. That row must not be
// attributed to the spelling.
//
// The guard has to read the per-mount key set, not the union's: the union holds
// the key config-drift (alpha's), so checking it would accept exactly this row.
// alpha is unrebuilt, so it has no row of its own for the spelling to win with.
func TestLensMotifCluster_AliasAttributionIsPerMount(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	alpha, beta := env.m.Get("alpha"), env.m.Get("beta")
	writeFactsInOneCommit(t, alpha,
		plainFact("kb/a.md", "alpha drift", withMotifs("config-drift")),
		plainFact("kb/b.md", "alpha drifts", withMotifs("config-drifts")))
	writeCosFact(t, beta, "kb/x.md", "beta once", 0.9, withMotifs("config-drifts", "config-skew"))
	judgeMerge(t, beta, "config-drifts", "config-skew", "beta's own unrelated merge")
	writeCosFact(t, beta, "kb/x.md", "beta now", 0.9, withMotifs("unrelated-thing"))

	// Positive control: beta's alias table really holds the row this test is
	// about, under the key the lens cluster has.
	withMotifIndex(t, beta, func(mi *vocab.MotifIndex, branch string) {
		rows, err := mi.AliasRows(context.Background(), branch)
		require.NoError(t, err)
		require.Equal(t, "config-drift", rows["config-drifts"].ClusterKey)
		require.Equal(t, "beta's own unrelated merge", rows["config-drifts"].Rationale)
	})

	body := decodeMotifDetail(t, env.get(t, "/lenses/eng/motifs/config-drift"))
	require.Equal(t, []string{"config-drift", "config-drifts"}, body.Members, "alpha's cluster alone")
	require.Equal(t, map[string]string{"config-drift": "/", "config-drifts": "/"}, body.aliasesByMotif(),
		"a non-contributing mount's audit trail must not be taken")
}
