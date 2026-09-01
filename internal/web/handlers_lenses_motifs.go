package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// The lens-wide motif vocabulary lives in internal/federate (MotifUnion), not
// here, because TWO surfaces answer motif queries against a lens: this REST
// group and the MCP knomit_query fan-out. This file holds only what is a
// rendering concern — the definition election and the pooled health block —
// and reaches for federate for everything that decides which spellings are one
// shape.

// motifDefinitionOf elects the definition shown for a MERGED cluster: the
// freshest across mounts (current beats stale beats missing), ties broken
// write-mount-first then binding order.
//
// Kept here rather than in the union because a definition is a thing the web
// surface DISPLAYS; the union's job is which spellings are one shape. It reads
// each mount's own constituent key via ContributedKey, never the union's key
// set — a key another mount coined can name an unrelated cluster on this one.
func motifDefinitionOf(
	g *federate.MotifGroup, targets []federate.Target, write *repos.RepoInstance,
	defsByMount []map[string]store.MotifDefinitionStatus,
) (store.MotifDefinitionStatus, bool) {
	var best store.MotifDefinitionStatus
	bestRank, bestWrite := 0, false
	for i, t := range targets {
		isWrite := t.RT.RI == write
		for key, st := range defsByMount[i] {
			if !g.ContributedKey(t.RT.RI, key) {
				continue
			}
			rank := definitionRank(st, true)
			if rank > bestRank || (rank == bestRank && isWrite && !bestWrite) {
				best, bestRank, bestWrite = st, rank, isWrite
			}
		}
	}
	return best, bestRank > 0
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

// gatherLensMotifs reads every target's vocabulary into one merged union, and
// (optionally) each mount's definitions and pooled health beside it.
//
// COST, and a FAILURE COUPLING. This is a per-mount Clusters read on every
// call, and there is no cache behind it. Affordable on the vocabulary block and the picker, which fire once
// per panel; worth knowing about on the cluster detail, which runs once PER
// MOTIF on an opened fact — a 3-motif fact in a 5-mount lens is 15 Clusters
// reads. Anything more would be a cache, and a cache would need an invalidation
// story per mount per branch. (The motif-filter expansion path shares the same
// union machinery but goes through federate.ExpandMotifTerms, which reads
// clusters only and skips a single-mount binding entirely.)
//
// Any mount error fails the WHOLE request (RFC §9.1 — a lens never silently
// shrinks its read set): returning the surviving mounts' vocabulary would
// present a smaller union as if it were the whole one, which no field in the
// response is allowed to disclose. On that path it writes the problem response
// and returns ok=false.
//
// On the FILTER path (federate.ExpandMotifTerms) that rule reaches further than
// it used to, and it is worth saying plainly: because a term's meaning is
// lens-wide, the expansion reads EVERY mount even for a `repo=`-narrowed
// request, so a mount the caller excluded can fail their query. That follows
// from §9.1 — the term is resolved against the lens, and a lens does not answer
// from a shrunken read set — but it is a coupling a narrowed query did not have
// before this shipped.
func gatherLensMotifs(
	w http.ResponseWriter, r *http.Request, bind *repos.Binding,
	provider motifsProvider, targets []federate.Target, withDefs, withHealth bool,
) (*federate.MotifUnion, []map[string]store.MotifDefinitionStatus, store.MotifVocabularyHealth, bool) {
	var pooled store.MotifVocabularyHealth
	defsByMount := make([]map[string]store.MotifDefinitionStatus, len(targets))
	u := federate.NewMotifUnion()
	for i, t := range targets {
		clusters, err := provider.Clusters(r.Context(), t.RT.RI, t.RT.Branch, t.Path)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load motif vocabulary", t.RT.Branch)
			return nil, nil, pooled, false
		}
		if withDefs {
			defs, err := provider.Definitions(r.Context(), t.RT.RI, t.RT.Branch, motifClusterKeys(clusters))
			if err != nil {
				writeStoreError(w, r, err, "Failed to load motif vocabulary", t.RT.Branch)
				return nil, nil, pooled, false
			}
			defsByMount[i] = defs
		}
		if withHealth {
			h, err := provider.VocabularyHealth(r.Context(), t.RT.RI, t.RT.Branch, t.Path)
			if err != nil {
				writeStoreError(w, r, err, "Failed to load motif vocabulary", t.RT.Branch)
				return nil, nil, pooled, false
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
			u.Add(t.RT.RI, c)
		}
	}
	return u, defsByMount, pooled, true
}

// expandLensMotifs widens a caller's motif terms through the lens's merged
// vocabulary before a filtered read fans out. A thin adapter over
// federate.ExpandMotifTerms — the ONE definition the MCP fan-out shares, so a
// chip minted anywhere is correct because the seam resolves it, not because
// the caller remembered to send members. See that function for the semantics
// this establishes and the failure coupling it accepts.
//
// On error it writes the problem response and returns ok=false.
func expandLensMotifs(
	w http.ResponseWriter, r *http.Request, bind *repos.Binding,
	provider motifsProvider, terms []string,
) ([]string, bool) {
	if len(terms) == 0 || provider == nil {
		return terms, true
	}
	read := func(ctx context.Context, rt repos.ReadTarget) ([]store.MotifCluster, error) {
		// Branch-wide: identity is not scoped by where the reader stands.
		return provider.Clusters(ctx, rt.RI, rt.Branch, "")
	}
	out, err := federate.ExpandMotifTerms(r.Context(), bind, read, terms)
	if err != nil {
		// The failing mount's branch, not "": writeStoreError renders
		// store.ErrBranchNotFound as `no branch named "<branch>"`, and this is
		// the fan-out where a mount pinned to a deleted branch actually shows
		// up. An empty branch there would print the message with the one word
		// it exists to carry missing.
		var readErr *federate.MotifReadError
		branch := ""
		if errors.As(err, &readErr) {
			branch = readErr.Branch
		}
		writeStoreError(w, r, err, "Failed to resolve motif filter", branch)
		return nil, false
	}
	return out, true
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

		u, defsByMount, health, ok := gatherLensMotifs(w, r, bind, provider, targets, true, true)
		if !ok {
			return
		}

		// Render the merged vocabulary through the SAME renderer the per-repo
		// endpoint uses. `defs` carries presence as meaning, exactly as the
		// per-mount bulk read does: a key absent from it has no definition on
		// any mount.
		groups := u.Groups()
		clusters := make([]store.MotifCluster, 0, len(groups))
		defs := make(map[string]store.MotifDefinitionStatus, len(groups))
		for _, g := range groups {
			key := g.Key()
			clusters = append(clusters, store.MotifCluster{
				ClusterKey:  key,
				CanonicalID: g.Canonical,
				Members:     g.Members(),
				DF:          g.DF,
				DFTotal:     g.DFTotal,
			})
			if st, defined := motifDefinitionOf(g, targets, bind.Write(), defsByMount); defined {
				defs[key] = st
			}
		}

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

		u, defsByMount, _, ok := gatherLensMotifs(w, r, bind, provider, targets, true, false)
		if !ok {
			return
		}
		group, found := u.Lookup(rawKey)
		if !found {
			hal.WriteProblem(w, http.StatusNotFound, "Unknown motif",
				`no motif cluster or spelling "`+rawKey+`" in this lens's vocabulary`, r.URL.Path)
			return
		}
		key, members := group.Key(), group.Members()
		def, defined := motifDefinitionOf(group, targets, bind.Write(), defsByMount)

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
					// Only a row whose own cluster key is one THIS MOUNT
					// contributed to the merge. The union's `keys` would be the
					// wrong set: it holds keys other mounts coined, and a key
					// coined elsewhere can name an unrelated cluster here — a
					// cluster key is a mechanical function of a spelling, so two
					// corpora can reach the same key with different membership.
					// Checking the union set would attribute another mount's
					// audit trail to a spelling this one files somewhere else.
					if row, ok := rows[m]; ok && group.ContributedKey(t.RT.RI, row.ClusterKey) {
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
			Canonical:       group.Canonical,
			Members:         members,
			DF:              group.DF,
			DFTotal:         group.DFTotal,
			Definition:      def.Definition,
			DefinitionState: definitionState(def, defined),
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
