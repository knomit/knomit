package web

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// ─── the union ───────────────────────────────────────────────────────────────
//
// A motif cluster_key is a MECHANICAL function of a spelling (store's
// groupingKey: canonicalize → tokenize → sort → join), so the same key names
// the same shape in every repo and merging by key is sound. It is not
// SUFFICIENT, though: a judge merge is per-branch, so one mount can have pulled
// `quiet-degradation` under the key `fallback-silent` while another still keys
// it `degradation-quiet`. Merging by key alone would then emit that one
// spelling in two rows of one vocabulary, and the two rows would pivot to
// different lists from names the reader cannot tell apart.
//
// So per-mount clusters are unioned TRANSITIVELY: two clusters are the same
// cluster when they share a cluster_key OR share any member spelling (within a
// mount a spelling belongs to exactly one cluster, so a shared spelling is
// evidence the two are one shape). That rule is also what makes the pivot
// correct by construction — the merged member list IS the `motifs=` CSV the
// detail's _links.facts sends, and every mount expands that union against its
// own alias table, which is exactly what the lens search/facts fan-out already
// does with a caller-supplied motif filter.

// motifGroup is one merged cluster under construction.
type motifGroup struct {
	// keys is every constituent cluster key, from every mount. The merged
	// cluster is addressed by the smallest of them (see clusterKey).
	keys    map[string]bool
	members map[string]bool
	// df / dfTotal are per-mount SUMS. Mounts are distinct repos, but distinct
	// is not disjoint: a re-rooted fork mounted beside its upstream shares
	// server-generated fact UUIDs, so one fact can be counted twice. That is
	// the same accepted over-count the /lenses/{lens}/stats histograms make,
	// for the same reason — an aggregate carries no per-fact paths to dedupe
	// on (kb/gotchas/lens/browsing-ui-accepted-gaps). The carrier list on the
	// detail DOES carry paths, and dedupes exactly.
	df      int
	dfTotal int
	// canonical is ELECTED, mirroring store.electCanonical at the granularity
	// the wire gives us: the representative of the highest-df constituent,
	// ties broken lexicographically. Deterministic, and independent of mount
	// order.
	canonical string
	bestDF    int
	// The definition shown is the FRESHEST across mounts: current beats stale
	// beats missing, ties broken write-mount-first then binding order.
	def      store.MotifDefinitionStatus
	defRank  int // 0 none, 1 stale, 2 current
	defWrite bool
}

// definitionRank scores a mount's definition for the election above.
func definitionRank(st store.MotifDefinitionStatus, ok bool) int {
	switch {
	case !ok:
		return 0
	case st.Stale:
		return 1
	default:
		return 2
	}
}

// motifUnion merges per-mount vocabularies into one lens-wide vocabulary.
//
// Union-find over group indices: byKey and byMember can point at an absorbed
// group, and find() walks to the survivor. Absorption always goes into the
// LOWER index, so the result does not depend on which order two groups happened
// to meet.
type motifUnion struct {
	groups   []*motifGroup
	parent   []int
	byKey    map[string]int
	byMember map[string]int
}

func newMotifUnion() *motifUnion {
	return &motifUnion{byKey: map[string]int{}, byMember: map[string]int{}}
}

func (u *motifUnion) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

// absorb folds group b into group a (a < b), applying every election rule.
func (u *motifUnion) absorb(a, b int) {
	ga, gb := u.groups[a], u.groups[b]
	for k := range gb.keys {
		ga.keys[k] = true
	}
	for m := range gb.members {
		ga.members[m] = true
	}
	ga.df += gb.df
	ga.dfTotal += gb.dfTotal
	if gb.bestDF > ga.bestDF || (gb.bestDF == ga.bestDF && gb.canonical < ga.canonical) {
		ga.canonical, ga.bestDF = gb.canonical, gb.bestDF
	}
	if gb.defRank > ga.defRank || (gb.defRank == ga.defRank && gb.defWrite && !ga.defWrite) {
		ga.def, ga.defRank, ga.defWrite = gb.def, gb.defRank, gb.defWrite
	}
	u.parent[b] = a
	u.groups[b] = nil
}

// add folds one mount's cluster into the union.
func (u *motifUnion) add(c store.MotifCluster, def store.MotifDefinitionStatus, defined, isWrite bool) {
	g := &motifGroup{
		keys:      map[string]bool{c.ClusterKey: true},
		members:   map[string]bool{},
		df:        c.DF,
		dfTotal:   c.DFTotal,
		canonical: c.CanonicalID,
		bestDF:    c.DF,
		def:       def,
		defRank:   definitionRank(def, defined),
		defWrite:  isWrite,
	}
	for _, m := range c.Members {
		g.members[m] = true
	}
	idx := len(u.groups)
	u.groups = append(u.groups, g)
	u.parent = append(u.parent, idx)

	// Every existing group this cluster touches, by key or by any spelling.
	touched := map[int]bool{}
	if i, ok := u.byKey[c.ClusterKey]; ok {
		touched[u.find(i)] = true
	}
	for _, m := range c.Members {
		if i, ok := u.byMember[m]; ok {
			touched[u.find(i)] = true
		}
	}
	root := idx
	for other := range touched {
		a, b := min(root, other), max(root, other)
		if a == b {
			continue
		}
		u.absorb(a, b)
		root = a
	}
	// Re-point every index this group owns at the survivor. Cheap, and it
	// keeps find() shallow.
	g = u.groups[root]
	for k := range g.keys {
		u.byKey[k] = root
	}
	for m := range g.members {
		u.byMember[m] = root
	}
}

// clusterKey is a merged cluster's identity: the lexicographically smallest
// constituent key.
//
// Smallest rather than, say, the highest-df mount's, because a cluster key is
// what definitions and URLs hang off and the store's own rule is that it must
// be STABLE UNDER DF CHANGE. min() is that, is deterministic, does not depend
// on mount order, and for a lens over one repo is the repo's own key — so a
// lens of one answers with the vocabulary of the repo it wraps, unchanged.
func (g *motifGroup) clusterKey() string {
	key := ""
	for k := range g.keys {
		if key == "" || k < key {
			key = k
		}
	}
	return key
}

func (g *motifGroup) sortedMembers() []string {
	out := make([]string, 0, len(g.members))
	for m := range g.members {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// resolve renders the merged vocabulary in the store's own order (df-desc,
// canonical-asc), plus the definition map keyed by merged cluster key —
// presence meaning "some mount had one", exactly as the per-repo Definitions
// bulk read means it.
func (u *motifUnion) resolve() ([]store.MotifCluster, map[string]store.MotifDefinitionStatus) {
	clusters := make([]store.MotifCluster, 0, len(u.groups))
	defs := make(map[string]store.MotifDefinitionStatus, len(u.groups))
	for i, g := range u.groups {
		if g == nil || u.find(i) != i {
			continue
		}
		key := g.clusterKey()
		clusters = append(clusters, store.MotifCluster{
			ClusterKey:  key,
			CanonicalID: g.canonical,
			Members:     g.sortedMembers(),
			DF:          g.df,
			DFTotal:     g.dfTotal,
		})
		if g.defRank > 0 {
			defs[key] = g.def
		}
	}
	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].DF != clusters[j].DF {
			return clusters[i].DF > clusters[j].DF
		}
		return clusters[i].CanonicalID < clusters[j].CanonicalID
	})
	return clusters, defs
}

// lookup resolves a raw {key} path segment to a merged group. It accepts the
// merged cluster key, ANY mount's constituent key, and any member spelling —
// all three name the same cluster, and a reader arriving from a repo-scoped
// link holds the second kind.
//
// No fallback to the store's per-branch ClusterKey resolution, unlike the repo
// detail: Clusters returns members straight from fact_motifs, so every spelling
// carried by a live fact is already a member here, and a spelling carried by no
// live fact resolves to a key with no cluster — a 404 either way.
func (u *motifUnion) lookup(raw string) (*motifGroup, bool) {
	if i, ok := u.byKey[raw]; ok {
		return u.groups[u.find(i)], true
	}
	if i, ok := u.byMember[raw]; ok {
		return u.groups[u.find(i)], true
	}
	return nil, false
}

// gatherLensMotifs fans the vocabulary read out over targets and folds every
// mount's clusters into one union, pooling the health counts as it goes.
//
// Any mount error fails the WHOLE request (RFC §9.1 — a lens never silently
// shrinks its read set): returning the surviving mounts' vocabulary would
// present a smaller union as if it were the whole one, which no field in the
// response is allowed to disclose (kb/decisions/lens/no-federation-metadata-in-responses).
// On that path it writes the problem response and returns ok=false.
func gatherLensMotifs(
	w http.ResponseWriter, r *http.Request, bind *repos.Binding,
	provider motifsProvider, targets []federate.Target, withHealth bool,
) (*motifUnion, store.MotifVocabularyHealth, bool) {
	u := newMotifUnion()
	var pooled store.MotifVocabularyHealth
	for _, t := range targets {
		isWrite := t.RT.RI == bind.Write()
		clusters, err := provider.Clusters(r.Context(), t.RT.RI, t.RT.Branch, t.Path)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif vocabulary", t.RT.Branch)
			return nil, pooled, false
		}
		defs, err := provider.Definitions(r.Context(), t.RT.RI, t.RT.Branch, motifClusterKeys(clusters))
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif vocabulary", t.RT.Branch)
			return nil, pooled, false
		}
		if withHealth {
			h, err := provider.VocabularyHealth(r.Context(), t.RT.RI, t.RT.Branch, t.Path)
			if err != nil {
				writeStoreError(w, r, err, "Failed to load motif vocabulary", t.RT.Branch)
				return nil, pooled, false
			}
			// POOLED counts. The two ratios are derived from the pooled totals
			// exactly ONCE, below — never averaged across mounts. Averaging
			// ratios answers a different question and gets a different number:
			// two mounts at 8/4 and 2/16 average to 1.0625 while the vocabulary
			// they jointly describe is at 10/20 = 0.5. This is the same move
			// handleHALLensStats makes for AxisFromSeparation: sum the raw
			// counters, apply the rule once.
			pooled.Clusters += h.Clusters
			pooled.Recurring += h.Recurring
			pooled.Mints += h.Mints
			pooled.Links += h.Links
			pooled.EpistemicRecurring += h.EpistemicRecurring
		}
		for _, c := range clusters {
			st, defined := defs[c.ClusterKey]
			u.add(c, st, defined, isWrite)
		}
	}
	return u, pooled, true
}

// ─── collection ──────────────────────────────────────────────────────────────

// handleHALLensMotifs serves GET /lenses/{lens}/motifs — the motif vocabulary
// of a lens's write repo + N read mounts, merged into one list.
//
// It is the lens twin of handleHALMotifs and shares its query parsing, its
// narrowing/ordering/paging and its envelope (motifCollectionQuery +
// renderMotifCollection), so the two surfaces answer with the same shape by
// construction rather than by being edited in step. The lens-only parts are the
// fan-out, the cluster union, and the pooled health block.
//
// This is the surface the v1 motif work deliberately left out — MotifsBlock's
// header said "there is no single vocabulary across a lens". There is one now,
// and this is its definition.
func handleHALLensMotifs(b hal.URLBuilder, provider motifsProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bind := repos.BindingFromContext(r.Context())
		lensName := chi.URLParam(r, "lens")

		p, ok := motifCollectionQuery(w, r)
		if !ok {
			return
		}

		// Ontology-aware fan-out target selection — the same seam every other
		// lens read uses. ?path= is SCOPE here exactly as it is on the repo
		// endpoint, so it reaches each mount's Clusters AND VocabularyHealth.
		targets, err := federate.ReadTargetsFor(bind, p.path)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}
		// Optional repeatable `repo=<mount name>` narrows the fan-out (422 on
		// an unknown mount name) — the shared lens union-read filter.
		targets, ok = narrowByRepo(w, r, bind, targets, r.URL.Query()["repo"])
		if !ok {
			return
		}

		u, health, ok := gatherLensMotifs(w, r, bind, provider, targets, true)
		if !ok {
			return
		}
		clusters, defs := u.resolve()

		renderMotifCollection(w, r, b.Lens(lensName)+"/motifs", clusters, defs, health, p)
	}
}

// ─── cluster detail ──────────────────────────────────────────────────────────

// handleHALLensMotifCluster serves GET /lenses/{lens}/motifs/{key} — one
// merged cluster, with a carrier preview drawn from every mount.
//
// {key} accepts the merged cluster_key, any mount's own cluster_key, or any
// member spelling; the self link always carries the merged key (C1). 404 only
// when nothing in the union answers to it.
//
// Branch-wide on every mount, and no ?repo= narrowing: a cluster is addressed
// by IDENTITY here, and its identity is a property of the lens, not of where
// the reader was standing or which mounts they had selected. The repo detail
// passes "" for the same reason.
func handleHALLensMotifCluster(b hal.URLBuilder, provider motifsProvider, facts factsCollectionProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bind := repos.BindingFromContext(r.Context())
		lensName := chi.URLParam(r, "lens")
		rawKey := chi.URLParam(r, "key")

		carrierLimit, ok := motifCarrierLimit(w, r)
		if !ok {
			return
		}

		targets, err := federate.ReadTargetsFor(bind, "")
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}

		u, _, ok := gatherLensMotifs(w, r, bind, provider, targets, false)
		if !ok {
			return
		}
		group, found := u.lookup(rawKey)
		if !found {
			hal.WriteProblem(w, http.StatusNotFound, "Unknown motif",
				`no motif cluster or spelling "`+rawKey+`" in this lens's vocabulary`, r.URL.Path)
			return
		}
		key, members := group.clusterKey(), group.sortedMembers()

		// Carriers ARE the pivot: the same filter, the same tier, the same code
		// path as /lenses/{lens}/facts?motifs=<members>&motif_match=exact, so
		// the preview cannot disagree with the listing it links to. Passing
		// every MERGED spelling is what makes the union correct per mount: each
		// mount expands only what its own alias table knows, and the union of
		// spellings is what covers the shapes it has not merged yet.
		lists := make([][]store.RecentFactEntry, len(targets))
		stamps := make([][]int64, len(targets))
		total, fetched := 0, 0
		truncated := false
		for i, t := range targets {
			entries, n, err := facts.RecentFacts(r.Context(), t.RT.RI, t.RT.Branch, store.SearchOptions{
				Motifs:     members,
				MotifMatch: store.MotifMatchExact,
				Limit:      carrierLimit,
			})
			if err != nil {
				writeStoreError(w, r, err, "Failed to load motif carriers", t.RT.Branch)
				return
			}
			lists[i] = entries
			total += n
			fetched += len(entries)
			if len(entries) < n {
				truncated = true
			}
			stamps[i] = make([]int64, len(entries))
			for j, e := range entries {
				stamps[i][j] = e.CommittedAt
			}
		}

		// Dedupe by repo-relative path through the ONE definition every lens
		// union surface shares (write mount wins, then binding order), then
		// merge by committed_at — comparable across mounts, unlike a relevance
		// rank, and this preview has no text query.
		winner := federate.WriteFirstWinners(targets, bind.Write(), lists,
			func(e store.RecentFactEntry) string { return e.Path })
		carriers := make([]motifCarrierItem, 0, min(fetched, carrierLimit))
		deduped := 0
		for _, ref := range federate.MergeRecent(stamps, fetched) {
			e := lists[ref.Mount][ref.Rank]
			if winner[e.Path] != ref.Mount {
				continue
			}
			deduped++
			if len(carriers) >= carrierLimit {
				continue
			}
			// Qualified HERE, after the dedupe and never before: dedupe exists
			// because one path can live on two mounts (a re-rooted fork shares
			// fact UUIDs), and keying on qualified paths would give the copies
			// different keys, so both would survive and the write mount would
			// stop winning. A bare read-mount path on the wire resolves against
			// the WRITE repo and 404s when opened — the /stats highlights
			// incident, which this row shape would otherwise repeat.
			wire := lensWirePath(bind, targets[ref.Mount].RT, e.Path)
			carriers = append(carriers, motifCarrierItem{
				Path:        wire,
				Title:       e.Title,
				Type:        e.Type,
				CommittedAt: e.CommittedAt,
				// PathEscape, unlike the repo builder's raw append: a qualified
				// path is kb://<id12>/… and its "//" would otherwise be a path
				// segment boundary. This is the escaping the web client already
				// uses on /lenses/{lens}/facts/{path}.
				Links: hal.LinkMap{"self": {Href: b.Lens(lensName) + "/facts/" + url.PathEscape(wire)}},
			})
		}

		// carrier_count counts MATCHES, never rows transferred
		// (kb/invariants/web/collections/count-vs-transfer). When no mount
		// truncated we hold every match, so the deduped union length IS the
		// cardinality — forks and all. When one did, the overlap is unknowable
		// without fetching the rest, and the summed per-mount count is the best
		// available answer: an upper bound, off by exactly the number of
		// cross-mount path collisions. Same trade, same reasoning, as the lens
		// facts collection's total.
		carrierCount := deduped
		if truncated {
			carrierCount = total
		}

		// Alias rows: one per merged member, taking the audit trail from the
		// mount that actually recorded a decision about that spelling — write
		// mount first, then binding order, mirroring WriteFirstWinners' rule
		// (Reads() is name-sorted, so the write mount is not positionally
		// first and has to be prioritised explicitly).
		rowFor := map[string]store.AliasRow{}
		for _, pass := range []bool{true, false} {
			for _, t := range targets {
				if (t.RT.RI == bind.Write()) != pass {
					continue
				}
				rows, err := provider.AliasRows(r.Context(), t.RT.RI, t.RT.Branch)
				if err != nil {
					writeStoreError(w, r, err, "Failed to load motif cluster", t.RT.Branch)
					return
				}
				for _, m := range members {
					if _, taken := rowFor[m]; taken {
						continue
					}
					// Only a row whose own cluster key is one this merged
					// cluster is made of: a spelling that a mount files under
					// some other cluster is not evidence about this one.
					if row, ok := rows[m]; ok && group.keys[row.ClusterKey] {
						rowFor[m] = row
					}
				}
			}
		}
		aliases := make([]motifAliasItem, 0, len(members))
		for _, m := range members {
			item := motifAliasItem{Motif: m}
			if row, ok := rowFor[m]; ok {
				item.Method = row.Method
				item.Rationale = row.Rationale
			}
			aliases = append(aliases, item)
		}

		pivot := url.Values{}
		pivot.Set("motifs", strings.Join(members, ","))
		pivot.Set("motif_match", string(store.MotifMatchExact))

		hal.WriteHAL(w, http.StatusOK, motifDetailView{
			ClusterKey:      key,
			Canonical:       group.canonical,
			Members:         members,
			DF:              group.df,
			DFTotal:         group.dfTotal,
			Definition:      group.def.Definition,
			DefinitionState: definitionState(group.def, group.defRank > 0),
			CarrierCount:    carrierCount,
			Carriers:        carriers,
			Aliases:         aliases,
			Links: hal.LinkMap{
				"self":  {Href: b.Lens(lensName) + "/motifs/" + key},
				"facts": {Href: b.Lens(lensName) + "/facts?" + pivot.Encode()},
			},
		})
	}
}
