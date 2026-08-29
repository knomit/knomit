package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// motifsProvider is the narrow interface the motif vocabulary handlers depend
// on. Every method is a straight pass-through to store.MotifIndex — the seam
// exists for test injection, mirroring domainsProvider.
type motifsProvider interface {
	Clusters(ctx context.Context, ri *repos.RepoInstance, branch string) ([]store.MotifCluster, error)
	Definitions(ctx context.Context, ri *repos.RepoInstance, branch string, keys []string) (map[string]store.MotifDefinitionStatus, error)
	VocabularyHealth(ctx context.Context, ri *repos.RepoInstance, branch string) (store.MotifVocabularyHealth, error)
	AliasRows(ctx context.Context, ri *repos.RepoInstance, branch string) (map[string]store.AliasRow, error)
	ClusterKey(ctx context.Context, ri *repos.RepoInstance, branch, motif string) (string, error)
}

type defaultMotifsProvider struct{}

func (defaultMotifsProvider) Clusters(ctx context.Context, ri *repos.RepoInstance, branch string) ([]store.MotifCluster, error) {
	var (
		out []store.MotifCluster
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Motifs().Clusters(ctx, branch)
	})
	return out, err
}

func (defaultMotifsProvider) Definitions(ctx context.Context, ri *repos.RepoInstance, branch string, keys []string) (map[string]store.MotifDefinitionStatus, error) {
	var (
		out map[string]store.MotifDefinitionStatus
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Motifs().Definitions(ctx, branch, keys)
	})
	return out, err
}

func (defaultMotifsProvider) VocabularyHealth(ctx context.Context, ri *repos.RepoInstance, branch string) (store.MotifVocabularyHealth, error) {
	var (
		out store.MotifVocabularyHealth
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Motifs().VocabularyHealth(ctx, branch)
	})
	return out, err
}

func (defaultMotifsProvider) AliasRows(ctx context.Context, ri *repos.RepoInstance, branch string) (map[string]store.AliasRow, error) {
	var (
		out map[string]store.AliasRow
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Motifs().AliasRows(ctx, branch)
	})
	return out, err
}

func (defaultMotifsProvider) ClusterKey(ctx context.Context, ri *repos.RepoInstance, branch, motif string) (string, error) {
	var (
		out string
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Motifs().ClusterKey(ctx, branch, motif)
	})
	return out, err
}

// Paging bounds for the motifs collection. Its own knobs rather than
// limitParam's: a vocabulary page is one row per CLUSTER with members and a
// definition attached, a different weight class from fact rows. The MAXIMUM is
// a hard ceiling exactly as limitParam's is — over it is a 400, never a quiet
// smaller page.
const (
	motifsDefaultLimit = 50
	motifsMaxLimit     = 200
)

// motifEntry is one cluster in the vocabulary collection.
//
// cluster_key is the STABLE identity and what self links carry; canonical is
// the df-elected representative and merely what humans read (design C1).
type motifEntry struct {
	ClusterKey      string      `json:"cluster_key"`
	Canonical       string      `json:"canonical"`
	Members         []string    `json:"members"`
	DF              int         `json:"df"`
	Definition      string      `json:"definition,omitempty"`
	DefinitionState string      `json:"definition_state"`
	Links           hal.LinkMap `json:"_links"`
}

// motifHealthView is MotifVocabularyHealth on the wire, plus the two derived
// ratios so a browser header needs no arithmetic.
//
// Every count is PREFIXED with the population it measures, because this block
// sits beside `count` in the same envelope and the two are not the same
// number. VocabularyHealth is computed over AUTHORED facts only, while `count`
// comes from the vocabulary query and includes every origin; and `count` is
// narrowed by ?q= while these are not — health describes the corpus, not the
// page. Bare `clusters` next to `count` reads as a contradiction; the prefix
// is what makes the disagreement legible instead of a bug report.
type motifHealthView struct {
	AuthoredClusters  int `json:"authored_clusters"`
	AuthoredRecurring int `json:"authored_recurring"`
	AuthoredMints     int `json:"authored_mints"`
	AuthoredLinks     int `json:"authored_links"`
	// AuthoredEpistemicRecurring narrows the authored population AGAIN, by
	// kind: VocabularyHealth counts it with a `kind = 'epistemic'` CASE
	// evaluated INSIDE the same `origin = 'authored'` filter as every other
	// count here, so its population is authored AND epistemic — not epistemic
	// instead of authored. It is the number the activation floor reads, and it
	// diverges from AuthoredRecurring badly enough (measured up to 5x) that
	// the qualifier has to be on the wire.
	AuthoredEpistemicRecurring int `json:"authored_epistemic_recurring"`
	// The two ratios are derived from the authored counts above, so they carry
	// the same population as everything else in this block.
	RecurrenceRate  float64 `json:"recurrence_rate"`
	MintToLinkRatio float64 `json:"mint_to_link_ratio"`
}

// motifsView is the collection envelope. Not hal.CollectionView because the
// vocabulary carries a health summary beside the page.
type motifsView struct {
	Count    int                     `json:"count"`
	Health   motifHealthView         `json:"health"`
	Links    hal.LinkMap             `json:"_links"`
	Embedded map[string][]motifEntry `json:"_embedded"`
}

// definitionState renders a cluster's Definitions lookup as the wire enum.
func definitionState(st store.MotifDefinitionStatus, ok bool) string {
	switch {
	case !ok:
		return "missing"
	case st.Stale:
		return "stale"
	default:
		return "current"
	}
}

// handleHALMotifs serves GET /repos/{repo}/branches/{branch}/motifs — the
// per-repo motif vocabulary, one entry per cluster, df-desc by default.
func handleHALMotifs(b hal.URLBuilder, provider motifsProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}
		qp := r.URL.Query()

		sortBy := qp.Get("sort")
		if sortBy == "" {
			sortBy = "df"
		}
		if sortBy != "df" && sortBy != "name" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				`invalid sort value (accepted: "df", "name")`, r.URL.Path)
			return
		}
		limit := motifsDefaultLimit
		if v := qp.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			// A HARD CEILING, not a clamp, matching limitParam: a client that
			// asked for 500 and silently received 200 rows has no way to tell
			// that from "there were only 200". Split into limitParam's three
			// messages so the refusal says WHICH bound was missed and what it
			// is, rather than making the caller guess.
			switch {
			case err != nil:
				badParam(w, r, "invalid limit value")
				return
			case n < 1:
				badParam(w, r, "limit must be at least 1")
				return
			case n > motifsMaxLimit:
				badParam(w, r, "limit must not exceed "+strconv.Itoa(motifsMaxLimit))
				return
			}
			limit = n
		}
		offset := 0
		if v := qp.Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			switch {
			case err != nil:
				badParam(w, r, "invalid offset value")
				return
			case n < 0:
				badParam(w, r, "offset must not be negative")
				return
			}
			offset = n
		}

		clusters, err := provider.Clusters(r.Context(), ri, branch)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif vocabulary", branch)
			return
		}
		health, err := provider.VocabularyHealth(r.Context(), ri, branch)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif vocabulary", branch)
			return
		}

		// Definitions for every cluster in one bulk read. Fetched before
		// narrowing because ?q= matches definition text too.
		keys := make([]string, len(clusters))
		for i, c := range clusters {
			keys[i] = c.ClusterKey
		}
		defs, err := provider.Definitions(r.Context(), ri, branch, keys)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif vocabulary", branch)
			return
		}

		if q := strings.ToLower(strings.TrimSpace(qp.Get("q"))); q != "" {
			// A NEW slice, never clusters[:0]: filtering in place rewrites the
			// backing array the provider handed over, so a provider that
			// returns a cached or shared slice would be silently corrupted by
			// a read request.
			kept := make([]store.MotifCluster, 0, len(clusters))
			for _, c := range clusters {
				if motifClusterMatches(c, defs[c.ClusterKey].Definition, q) {
					kept = append(kept, c)
				}
			}
			clusters = kept
		}

		// Clusters arrives df-desc / canonical-asc from the store; only the
		// name sort re-orders.
		if sortBy == "name" {
			// Same reasoning as the narrowing above: sort a copy, never the
			// provider's own slice.
			sorted := make([]store.MotifCluster, len(clusters))
			copy(sorted, clusters)
			clusters = sorted
			sort.Slice(clusters, func(i, j int) bool {
				return clusters[i].CanonicalID < clusters[j].CanonicalID
			})
		}

		total := len(clusters)
		// Page bounds by REMAINDER, never by the sum offset+limit. An offset
		// near MaxInt clears the `n < 0` parse guard above and then wraps the
		// sum NEGATIVE, which passes every `< total` test written as a sum and
		// reaches the slice expression as a negative bound — a bounds panic on
		// an unauthenticated GET. `limit < total-offset` asks the same question
		// with operands that cannot overflow (offset is capped at total first).
		start := min(offset, total)
		end := total
		if limit < total-start {
			end = start + limit
		}

		motifsBase := b.Branch(repoName, a) + "/motifs"
		links := hal.LinkMap{"self": {Href: selfWithQuery(motifsBase, r)}}
		if end < total {
			nextQ := r.URL.Query()
			nextQ.Set("offset", strconv.Itoa(end))
			links["next"] = hal.Link{Href: motifsBase + "?" + nextQ.Encode()}
		}
		if offset > 0 {
			prevQ := r.URL.Query()
			prevQ.Set("offset", strconv.Itoa(max(offset-limit, 0)))
			links["prev"] = hal.Link{Href: motifsBase + "?" + prevQ.Encode()}
		}

		page := clusters[start:end]
		items := make([]motifEntry, 0, len(page))
		for _, c := range page {
			st, ok := defs[c.ClusterKey]
			items = append(items, motifEntry{
				ClusterKey:      c.ClusterKey,
				Canonical:       c.CanonicalID,
				Members:         c.Members,
				DF:              c.DF,
				Definition:      st.Definition,
				DefinitionState: definitionState(st, ok),
				Links:           hal.LinkMap{"self": {Href: motifsBase + "/" + c.ClusterKey}},
			})
		}

		hal.WriteHAL(w, http.StatusOK, motifsView{
			// Count is the post-narrowing total over EVERY origin; the health
			// block beside it is authored-only and unnarrowed. See
			// motifHealthView for why its fields carry the population prefix.
			Count: total,
			Health: motifHealthView{
				AuthoredClusters:           health.Clusters,
				AuthoredRecurring:          health.Recurring,
				AuthoredMints:              health.Mints,
				AuthoredLinks:              health.Links,
				AuthoredEpistemicRecurring: health.EpistemicRecurring,
				RecurrenceRate:             health.RecurrenceRate(),
				MintToLinkRatio:            health.MintToLinkRatio(),
			},
			Links:    links,
			Embedded: map[string][]motifEntry{"motifs": items},
		})
	}
}

// motifClusterMatches reports whether q (already lowercased) occurs in any
// member spelling or in the cluster's definition.
func motifClusterMatches(c store.MotifCluster, definition, q string) bool {
	for _, m := range c.Members {
		if strings.Contains(strings.ToLower(m), q) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(definition), q)
}

// Carrier preview bounds for the cluster detail. The preview reuses the exact
// pivot query (RecentFacts + Motifs filter), so the full list is always one
// _links.facts away. The maximum is a hard ceiling (400 above it), not a clamp.
const (
	motifCarriersDefaultLimit = 20
	motifCarriersMaxLimit     = 100
)

// motifCarrierItem is one carrier fact in the detail preview, recency-ordered
// like the /facts pivot it previews.
type motifCarrierItem struct {
	Path        string      `json:"path"`
	Title       string      `json:"title"`
	Type        string      `json:"type,omitempty"`
	CommittedAt int64       `json:"committed_at,omitempty"`
	Links       hal.LinkMap `json:"_links"`
}

// motifAliasItem is one member spelling with the audit trail of how it joined
// the cluster. Method/rationale are absent on an unresolved (singleton)
// corpus — the spelling is a member because nothing has grouped it yet.
type motifAliasItem struct {
	Motif     string `json:"motif"`
	Method    string `json:"method,omitempty"`
	Rationale string `json:"rationale,omitempty"`
}

// motifDetailView is the cluster detail envelope.
type motifDetailView struct {
	ClusterKey      string             `json:"cluster_key"`
	Canonical       string             `json:"canonical"`
	Members         []string           `json:"members"`
	DF              int                `json:"df"`
	Definition      string             `json:"definition,omitempty"`
	DefinitionState string             `json:"definition_state"`
	CarrierCount    int                `json:"carrier_count"`
	Carriers        []motifCarrierItem `json:"carriers"`
	Aliases         []motifAliasItem   `json:"aliases"`
	Links           hal.LinkMap        `json:"_links"`
}

// handleHALMotifCluster serves GET /repos/{repo}/branches/{branch}/motifs/{key}.
//
// {key} accepts a cluster_key OR any member spelling; spellings resolve
// through the store's ClusterKey (which degrades to the mechanical key on an
// unrebuilt corpus — C2), and the self link always carries the cluster_key
// (C1). 404 only when the resolved key matches no cluster in the vocabulary.
func handleHALMotifCluster(b hal.URLBuilder, provider motifsProvider, facts factsCollectionProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}
		rawKey := chi.URLParam(r, "key")

		carrierLimit := motifCarriersDefaultLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			// Hard ceiling, same reasoning and same messages as the
			// collection's limit.
			switch {
			case err != nil:
				badParam(w, r, "invalid limit value")
				return
			case n < 1:
				badParam(w, r, "limit must be at least 1")
				return
			case n > motifCarriersMaxLimit:
				badParam(w, r, "limit must not exceed "+strconv.Itoa(motifCarriersMaxLimit))
				return
			}
			carrierLimit = n
		}

		clusters, err := provider.Clusters(r.Context(), ri, branch)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif cluster", branch)
			return
		}
		byKey := make(map[string]store.MotifCluster, len(clusters))
		for _, c := range clusters {
			byKey[c.ClusterKey] = c
		}

		cluster, found := byKey[rawKey]
		if !found {
			// Not a cluster key — treat it as a spelling and resolve.
			key, kerr := provider.ClusterKey(r.Context(), ri, branch, rawKey)
			if kerr != nil {
				writeStoreError(w, r, kerr, "Failed to load motif cluster", branch)
				return
			}
			cluster, found = byKey[key]
		}
		if !found {
			hal.WriteProblem(w, http.StatusNotFound, "Unknown motif",
				`no motif cluster or spelling "`+rawKey+`" in this branch's vocabulary`, r.URL.Path)
			return
		}

		defs, err := provider.Definitions(r.Context(), ri, branch, []string{cluster.ClusterKey})
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif cluster", branch)
			return
		}
		st, defined := defs[cluster.ClusterKey]

		// Carriers ARE the pivot: same filter, same recency order, same code
		// path as /facts?motifs=<members>&motif_match=exact, so the preview
		// cannot disagree with the full listing it links to. Passing every
		// member spelling keeps the union correct on an unrebuilt corpus,
		// where exact-tier resolution knows nothing about siblings (C2).
		entries, total, err := facts.RecentFacts(r.Context(), ri, branch, store.SearchOptions{
			Motifs:     cluster.Members,
			MotifMatch: store.MotifMatchExact,
			Limit:      carrierLimit,
		})
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif carriers", branch)
			return
		}
		carriers := make([]motifCarrierItem, 0, len(entries))
		for _, e := range entries {
			carriers = append(carriers, motifCarrierItem{
				Path:        e.Path,
				Title:       e.Title,
				Type:        e.Type,
				CommittedAt: e.CommittedAt,
				Links:       hal.LinkMap{"self": {Href: b.Fact(repoName, a, e.Path)}},
			})
		}

		aliasRows, err := provider.AliasRows(r.Context(), ri, branch)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif cluster", branch)
			return
		}
		aliases := make([]motifAliasItem, 0, len(cluster.Members))
		for _, m := range cluster.Members {
			item := motifAliasItem{Motif: m}
			if row, ok := aliasRows[m]; ok && row.ClusterKey == cluster.ClusterKey {
				item.Method = row.Method
				item.Rationale = row.Rationale
			}
			aliases = append(aliases, item)
		}

		motifsBase := b.Branch(repoName, a) + "/motifs"
		pivot := url.Values{}
		pivot.Set("motifs", strings.Join(cluster.Members, ","))
		pivot.Set("motif_match", string(store.MotifMatchExact))

		hal.WriteHAL(w, http.StatusOK, motifDetailView{
			ClusterKey:      cluster.ClusterKey,
			Canonical:       cluster.CanonicalID,
			Members:         cluster.Members,
			DF:              cluster.DF,
			Definition:      st.Definition,
			DefinitionState: definitionState(st, defined),
			CarrierCount:    total,
			Carriers:        carriers,
			Aliases:         aliases,
			Links: hal.LinkMap{
				"self":  {Href: motifsBase + "/" + cluster.ClusterKey},
				"facts": {Href: b.Branch(repoName, a) + "/facts?" + pivot.Encode()},
			},
		})
	}
}
