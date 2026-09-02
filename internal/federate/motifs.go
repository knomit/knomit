package federate

import (
	"context"
	"sort"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// The lens-wide motif vocabulary: every mount's clusters merged into one.
//
// A motif cluster_key is a MECHANICAL function of a spelling (store's
// groupingKey: canonicalize → tokenize → sort → join), so the same key names
// the same shape in every repo and merging by key is sound. It is not
// SUFFICIENT, though: a judge merge is per-branch, so one mount can have pulled
// `quiet-degradation` under the key `fallback-silent` while another still keys
// it `degradation-quiet`. Merging by key alone would then emit that one
// spelling in two rows of one vocabulary, and the two rows would answer
// different queries from names the reader cannot tell apart.
//
// So per-mount clusters are unioned TRANSITIVELY: two clusters are the same
// cluster when they share a cluster_key OR share any member spelling (within a
// mount a spelling belongs to exactly one cluster, so a shared spelling is
// evidence the two are one shape).
//
// THIS LIVES IN federate, not in a consumer, because two surfaces answer motif
// queries against a lens — the REST /lenses/{lens}/{facts,search,motifs} and
// the MCP knomit_query fan-out — and a second implementation kept in step is
// exactly the failure the union exists to prevent. The web surface shipped the
// widening first and MCP did not, which left the most-used motif path (recall)
// still dropping mounts; one definition here is what makes that impossible to
// repeat.

// MotifGroup is one merged cluster: what every mount contributed about one
// shape, resolved into a single row.
type MotifGroup struct {
	// keys is every constituent cluster key, from every mount. The merged
	// cluster is addressed by the smallest of them (see Key).
	keys    map[string]bool
	members map[string]bool
	// DF / DFTotal are per-mount SUMS. Mounts are distinct repos, but distinct
	// is not disjoint: a re-rooted fork mounted beside its upstream shares
	// server-generated fact UUIDs, so one fact can be counted twice. That is
	// the same accepted over-count the lens /stats histograms make, for the
	// same reason — an aggregate carries no per-fact paths to dedupe on.
	DF      int
	DFTotal int
	// Canonical is ELECTED, mirroring store.electCanonical at the granularity
	// a merged view has: the representative of the highest-df constituent,
	// ties broken lexicographically. Deterministic, and independent of the
	// order mounts were added in.
	Canonical string
	bestDF    int
	// mountKeys[ri] is the set of keys THAT MOUNT contributed, which is not
	// the same set as keys and must not be confused with it. keys is the union
	// across mounts, so it holds keys this mount never used — and a key another
	// mount coined can perfectly well name an UNRELATED cluster here, since a
	// cluster key is a mechanical function of a spelling and two corpora can
	// both hold that spelling in different company. Anything asking "did THIS
	// mount file this row under THIS cluster" has to read the per-mount set.
	mountKeys map[*repos.RepoInstance]map[string]bool
}

// Key is the merged cluster's identity: the lexicographically smallest
// constituent key.
//
// Smallest rather than, say, the highest-df mount's, because a cluster key is
// what definitions and URLs hang off and the store's own rule is that it must
// be STABLE UNDER DF CHANGE. min() is that, is deterministic, does not depend
// on mount order, and for a lens over one repo is that repo's own key — so a
// lens of one reports the vocabulary of the repo it wraps, unchanged.
func (g *MotifGroup) Key() string {
	key := ""
	for k := range g.keys {
		if key == "" || k < key {
			key = k
		}
	}
	return key
}

// Members is every spelling in the merged cluster, sorted.
func (g *MotifGroup) Members() []string {
	out := make([]string, 0, len(g.members))
	for m := range g.members {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ContributedKey reports whether ri filed this merged cluster under key. Use it
// — never the union's key set — when attributing per-mount state (an alias
// row's audit trail, say) to a merged row.
func (g *MotifGroup) ContributedKey(ri *repos.RepoInstance, key string) bool {
	return g.mountKeys[ri][key]
}

// MotifUnion merges per-mount vocabularies into one lens-wide vocabulary.
//
// Union-find over group indices: byKey and byMember can point at an absorbed
// group, and find() walks to the survivor. Absorption always goes into the
// LOWER index, so the result does not depend on which order two groups happened
// to meet.
type MotifUnion struct {
	groups   []*MotifGroup
	parent   []int
	byKey    map[string]int
	byMember map[string]int
}

// NewMotifUnion returns an empty union.
func NewMotifUnion() *MotifUnion {
	return &MotifUnion{byKey: map[string]int{}, byMember: map[string]int{}}
}

func (u *MotifUnion) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

// absorb folds group b into group a (a < b), applying every election rule.
func (u *MotifUnion) absorb(a, b int) {
	ga, gb := u.groups[a], u.groups[b]
	for k := range gb.keys {
		ga.keys[k] = true
	}
	for m := range gb.members {
		ga.members[m] = true
	}
	for ri, keys := range gb.mountKeys {
		if ga.mountKeys[ri] == nil {
			ga.mountKeys[ri] = map[string]bool{}
		}
		for k := range keys {
			ga.mountKeys[ri][k] = true
		}
	}
	ga.DF += gb.DF
	ga.DFTotal += gb.DFTotal
	if gb.bestDF > ga.bestDF || (gb.bestDF == ga.bestDF && gb.Canonical < ga.Canonical) {
		ga.Canonical, ga.bestDF = gb.Canonical, gb.bestDF
	}
	u.parent[b] = a
	u.groups[b] = nil
}

// Add folds one mount's cluster into the union.
func (u *MotifUnion) Add(ri *repos.RepoInstance, c store.MotifCluster) {
	g := &MotifGroup{
		keys:      map[string]bool{c.ClusterKey: true},
		members:   map[string]bool{},
		DF:        c.DF,
		DFTotal:   c.DFTotal,
		Canonical: c.CanonicalID,
		bestDF:    c.DF,
		mountKeys: map[*repos.RepoInstance]map[string]bool{ri: {c.ClusterKey: true}},
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

// Groups returns the merged vocabulary in the store's own order (df-desc,
// canonical-asc), so a merged list is ordered exactly as a per-repo one is.
func (u *MotifUnion) Groups() []*MotifGroup {
	out := make([]*MotifGroup, 0, len(u.groups))
	for i, g := range u.groups {
		if g == nil || u.find(i) != i {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DF != out[j].DF {
			return out[i].DF > out[j].DF
		}
		return out[i].Canonical < out[j].Canonical
	})
	return out
}

// Lookup resolves a raw term to a merged group. It accepts the merged cluster
// key, ANY mount's constituent key, and any member spelling — all three name
// the same cluster, and a reader arriving from a repo-scoped link holds the
// second kind.
func (u *MotifUnion) Lookup(raw string) (*MotifGroup, bool) {
	if i, ok := u.byKey[raw]; ok {
		return u.groups[u.find(i)], true
	}
	if i, ok := u.byMember[raw]; ok {
		return u.groups[u.find(i)], true
	}
	return nil, false
}

// ExpandTerms widens caller-supplied motif terms to the whole merged cluster
// each one names.
//
// A term in no cluster is left alone — its own singleton, which is what the
// store does with it too. Order-preserving and duplicate-free: two terms in one
// cluster (two chips a reader widened with) must not send the same spelling
// twice.
func (u *MotifUnion) ExpandTerms(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := make([]string, 0, len(terms))
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, term := range terms {
		g, found := u.Lookup(term)
		if !found {
			add(term)
			continue
		}
		for _, m := range g.Members() {
			add(m)
		}
	}
	return out
}

// MotifClusterReader reads one mount's resolved motif vocabulary. Both
// consumers already have a seam for this — the web handlers' motifsProvider and
// MCP's ri.WithRead — so the union takes the read as a function rather than
// growing an interface each caller would have to satisfy twice.
type MotifClusterReader func(ctx context.Context, rt repos.ReadTarget) ([]store.MotifCluster, error)

// MotifReadError names the mount whose vocabulary read failed.
//
// The mount has to travel with the error because the consumers' error responses
// are ABOUT the mount: the web layer turns store.ErrBranchNotFound into a 404
// reading `no branch named "<branch>"`, and a fan-out that dropped the branch
// would render that message as `no branch named ""` in exactly the case it
// exists for — a mount pinned to a deleted branch. Unwrap keeps errors.Is
// working through it, so the sentinel checks downstream are unaffected.
type MotifReadError struct {
	Repo   string
	Branch string
	Err    error
}

func (e *MotifReadError) Error() string {
	return "motif vocabulary read failed on mount " + e.Repo + "@" + e.Branch + ": " + e.Err.Error()
}

func (e *MotifReadError) Unwrap() error { return e.Err }

// BuildMotifUnion reads every target's vocabulary and merges it.
//
// Any mount error fails the WHOLE call (RFC §9.1 — a lens never silently
// shrinks its read set): a union missing a mount is a smaller vocabulary
// presented as the whole one, which no field in any response is allowed to
// disclose. The failure is returned as *MotifReadError so the caller can name
// the mount it came from.
func BuildMotifUnion(ctx context.Context, targets []Target, read MotifClusterReader) (*MotifUnion, error) {
	u := NewMotifUnion()
	for _, t := range targets {
		clusters, err := read(ctx, t.RT)
		if err != nil {
			return nil, &MotifReadError{Repo: t.RT.RI.Name(), Branch: t.RT.Branch, Err: err}
		}
		for _, c := range clusters {
			u.Add(t.RT.RI, c)
		}
	}
	return u, nil
}

// ExpandMotifTerms widens a caller's motif terms to the binding's MERGED
// clusters, so every mount is asked about the whole shape rather than about one
// mount's name for it.
//
// THIS IS THE CORRECTNESS SEAM FOR EVERY MOTIF-FILTERED LENS READ, and without
// it a lens query silently drops mounts its own vocabulary counted. The store's
// exact tier is per-branch canonical equality (store.expandMotifQuery): a term
// resolves through THAT branch's alias table, and a mount that carries the
// cluster under a different spelling — because the canonical is ELECTED per
// branch, and a judge merge is per-branch — resolves the term to itself, finds
// no member with that canonical, and contributes ZERO facts. A row reading
// "df 7" then answers with 5.
//
// SEMANTICS THIS ESTABLISHES, stated because it is a real behaviour change and
// compresses into a falsehood if left implicit: a judge merge on ANY mount
// changes what a motif term returns for the WHOLE lens. Read through a lens,
// the mounts share one vocabulary, exactly as one repo's folders share one —
// which is what "a lens answers like a single repo" means when the question is
// about motifs. It is READ-ONLY: no mount's alias table, canonical election or
// verdict history is touched, and the same repo read directly still answers
// with its own vocabulary alone.
//
// Expansion runs over the FULL read set, branch-wide, and callers must not pass
// a path- or repo-narrowed target list. Cluster identity is a property of the
// LENS, and a term minted from the whole vocabulary has to keep meaning the
// same shape when the list under it is narrowed — resolving through a narrowed
// union re-runs the exact bug this fixes, since the excluded mount may be the
// one spelling it differently.
//
// FAILURE COUPLING, new with this and worth knowing: because the read set is
// always the full one, a mount the caller EXCLUDED with repo= can still fail
// the request. That follows from the term being lens-wide (§9.1 says a lens
// never answers from a shrunken read set), but it is a coupling that did not
// exist before — a narrowed query used not to touch the mounts it narrowed
// away.
//
// ONE MOUNT IS SKIPPED, and that is a semantic choice, not an optimisation.
// Widening there would NOT be a no-op: a single branch's ClustersUnder already
// groups mechanically-equal spellings into one cluster (`config-drift` and
// `drift-config` sort to the same key), while the store's exact tier resolves a
// bare term through per-spelling canonicals — so on an unrebuilt corpus a
// widened term would reach cluster siblings that the same repo, read directly,
// does not. A lens over one repo must answer EXACTLY like that repo, so the
// widening starts at two mounts, where the union is the only thing that can say
// which spellings are one shape.
//
// Which does mean a lens over two mounts is looser at the exact tier than one
// repo is, on a corpus whose aliases have never been rebuilt. That is the same
// semantics stated above, seen from the other side: the union is the lens's
// vocabulary, and it says those spellings are one shape.
func ExpandMotifTerms(ctx context.Context, b *repos.Binding, read MotifClusterReader, terms []string) ([]string, error) {
	if len(terms) == 0 || read == nil {
		return terms, nil
	}
	targets, err := ReadTargetsFor(b, "")
	if err != nil {
		return nil, err
	}
	if len(targets) <= 1 {
		return terms, nil
	}
	u, err := BuildMotifUnion(ctx, targets, read)
	if err != nil {
		return nil, err
	}
	return u.ExpandTerms(terms), nil
}
