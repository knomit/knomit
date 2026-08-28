package web

import (
	"context"
	"net/http"
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
// definition attached, a different weight class from fact rows.
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
type motifHealthView struct {
	Clusters           int     `json:"clusters"`
	Recurring          int     `json:"recurring"`
	Mints              int     `json:"mints"`
	Links              int     `json:"links"`
	EpistemicRecurring int     `json:"epistemic_recurring"`
	RecurrenceRate     float64 `json:"recurrence_rate"`
	MintToLinkRatio    float64 `json:"mint_to_link_ratio"`
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
			if err != nil || n < 1 {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid limit value", r.URL.Path)
				return
			}
			limit = min(n, motifsMaxLimit)
		}
		offset := 0
		if v := qp.Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid offset value", r.URL.Path)
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
		motifsBase := b.Branch(repoName, a) + "/motifs"
		links := hal.LinkMap{"self": {Href: selfWithQuery(motifsBase, r)}}
		if offset+limit < total {
			nextQ := r.URL.Query()
			nextQ.Set("offset", strconv.Itoa(offset+limit))
			links["next"] = hal.Link{Href: motifsBase + "?" + nextQ.Encode()}
		}
		if offset > 0 {
			prevQ := r.URL.Query()
			prevQ.Set("offset", strconv.Itoa(max(offset-limit, 0)))
			links["prev"] = hal.Link{Href: motifsBase + "?" + prevQ.Encode()}
		}

		page := clusters[min(offset, total):min(offset+limit, total)]
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
			Count: total,
			Health: motifHealthView{
				Clusters:           health.Clusters,
				Recurring:          health.Recurring,
				Mints:              health.Mints,
				Links:              health.Links,
				EpistemicRecurring: health.EpistemicRecurring,
				RecurrenceRate:     health.RecurrenceRate(),
				MintToLinkRatio:    health.MintToLinkRatio(),
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
