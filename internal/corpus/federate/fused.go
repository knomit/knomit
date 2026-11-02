package federate

import (
	"context"
	"fmt"
	"sync"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/index"
)

// The fused lens read: ONE definition of how a lens answers a list query, shared
// by every surface that lists a lens — REST /lenses/{lens}/search and
// /lenses/{lens}/facts and MCP knomit_query (relevance and sort=recent). The
// lens FactSet's root search is PLANNED to call it too (F3, not built yet).
// Before it existed the REST handlers and the MCP tool each carried their own
// copy of the same pipeline, which is the mirrored-implementation shape that
// has already drifted twice on lens surfaces (a fix landing on one copy only).
// One function is what will make "the FactSet's roots are the lens list" true
// by construction rather than by being edited in step.
//
// The pipeline, in order:
//
//  1. PlanFused: widen motif terms over the FULL read set (ExpandMotifTerms),
//     select the fan-out targets (ReadTargetsFor), apply repo= narrowing
//     (NarrowByRepo), and compute the query vector ONCE for every mount.
//  2. The caller adjusts Plan.Opts for anything it validates after planning
//     (REST applies its expiry and context parameters here, so its error
//     precedence is unchanged).
//  3. FusedSearch / FusedRecent: fan out one caller-supplied LEG per target,
//     reduce in binding order, report the LOWEST-INDEXED failing mount, order
//     the union (RRF for relevance; committed_at merge for a text-less recency
//     listing, RRF when it has text), and drop every row whose repo-relative
//     path a higher-priority mount also holds (WriteFirstWinners: the write
//     mount first, then binding order).
//
// What is deliberately NOT here: parameter parsing, error rendering (each
// surface has its own problem documents / tool errors), page truncation (REST
// pages by offset/limit, MCP by max_results and a cursor), and how one mount is
// read. The leg is the caller's, which is also the seam a FactSet needs to read
// every mount at an anchor it froze: pass a leg that looks the anchor up by
// Target.Mount (the mount's stable index into Binding.Reads(), unchanged by
// narrowing) and reads Target.RT.RI at that anchor instead of at
// Target.RT.Branch.

// FusedStage names the planning step a PlanError came from, so each surface can
// render the failure the way it did before the pipeline was shared.
type FusedStage int

const (
	// StageMotifs: the motif vocabulary of some mount could not be read while
	// widening the caller's terms (the error wraps *MotifReadError when a
	// mount's read failed).
	StageMotifs FusedStage = iota + 1
	// StageTargets: the path filter is malformed or names an unmounted repo.
	StageTargets
	// StageRepos: repo= names a mount the lens does not have
	// (*UnknownRepoError).
	StageRepos
	// StageEmbed: the query text could not be embedded.
	StageEmbed
)

// PlanError is PlanFused's only error type. Error() is the underlying error's
// text unchanged, and Unwrap exposes it, so errors.Is / errors.As against the
// cause behave exactly as they would on the bare error.
type PlanError struct {
	Stage FusedStage
	Err   error
}

func (e *PlanError) Error() string { return e.Err.Error() }
func (e *PlanError) Unwrap() error { return e.Err }

// UnknownRepoError is a repo= value naming no mount of the lens.
type UnknownRepoError struct {
	Name string // the repo= value
	Lens string // the binding's name
}

func (e *UnknownRepoError) Error() string {
	return `no mount named "` + e.Name + `" in lens "` + e.Lens + `"`
}

// NarrowByRepo restricts targets to the mounts named in names (the optional,
// repeatable repo=<mount name> filter of the lens reads). A name the binding
// does not mount is an *UnknownRepoError — the write repo is itself a mount,
// so it is selectable by name. With no names the targets come back unchanged.
func NarrowByRepo(b *repos.Binding, targets []Target, names []string) ([]Target, error) {
	if len(names) == 0 {
		return targets, nil
	}
	known := make(map[string]bool, len(b.Reads()))
	for _, rt := range b.Reads() {
		known[rt.RI.Name()] = true
	}
	want := make(map[string]bool, len(names))
	for _, name := range names {
		if !known[name] {
			return nil, &UnknownRepoError{Name: name, Lens: b.Name()}
		}
		want[name] = true
	}
	kept := make([]Target, 0, len(targets))
	for _, t := range targets {
		if want[t.RT.RI.Name()] {
			kept = append(kept, t)
		}
	}
	return kept, nil
}

// FusedQuery is what a surface asks of a fused lens read.
type FusedQuery struct {
	Binding *repos.Binding
	// Path is the path filter, bare or kb://-qualified (ReadTargetsFor). Each
	// leg is handed its target's repo-relative form in Opts.Path.
	Path string
	// Repos is the repo= narrowing by mount name; nil or empty narrows nothing.
	Repos []string
	// Opts is the filter set every mount is asked with. Motifs are replaced by
	// their widened form and QueryVec by the shared vector; Path and Diag are
	// set per leg.
	Opts index.SearchOptions
	// Clusters reads one mount's motif vocabulary for the widening. nil skips
	// widening (ExpandMotifTerms treats a nil reader as the identity).
	Clusters MotifClusterReader
	// Embed computes the query vector of Opts.Text ONCE for the whole fan-out.
	// It returns (nil, nil) when there is nothing to embed (no text, no
	// embedder); every mount then embeds for itself. nil means the same.
	Embed func(ctx context.Context, text string) ([]float32, error)
}

// FusedPlan is a planned fused read: the targets the legs run against, in
// binding order, and the options each leg starts from. A caller may adjust
// Opts (a later-validated filter, a per-round Limit) before running it, and a
// FactSet may build one directly from targets it already holds.
type FusedPlan struct {
	Binding *repos.Binding
	Targets []Target
	Opts    index.SearchOptions
}

// PlanFused runs the planning half of the fused read, in this order: motif
// widening, target selection, repo= narrowing, the query vector. The first
// failing step returns a *PlanError naming it, and the later steps do not run.
//
// Widening runs over the FULL read set before any narrowing (a term's meaning
// is lens-wide; see ExpandMotifTerms), so a mount excluded by Path or Repos can
// still fail the request at StageMotifs.
func PlanFused(ctx context.Context, q FusedQuery) (*FusedPlan, error) {
	opts := q.Opts
	widened, err := ExpandMotifTerms(ctx, q.Binding, q.Clusters, opts.Motifs)
	if err != nil {
		return nil, &PlanError{Stage: StageMotifs, Err: err}
	}
	opts.Motifs = widened
	targets, err := ReadTargetsFor(q.Binding, q.Path)
	if err != nil {
		return nil, &PlanError{Stage: StageTargets, Err: err}
	}
	if targets, err = NarrowByRepo(q.Binding, targets, q.Repos); err != nil {
		return nil, &PlanError{Stage: StageRepos, Err: err}
	}
	if q.Embed != nil {
		vec, err := q.Embed(ctx, opts.Text)
		if err != nil {
			return nil, &PlanError{Stage: StageEmbed, Err: err}
		}
		opts.QueryVec = vec
	}
	return &FusedPlan{Binding: q.Binding, Targets: targets, Opts: opts}, nil
}

// MountError is one mount's failed leg: the LOWEST-INDEXED failing mount of the
// fan-out (b9c7ab96), never the first to finish. Error() is the leg's error
// text unchanged; Panicked marks a leg that panicked and was recovered, whose
// Err reads "mount <name>@<branch> panicked: <value>".
type MountError struct {
	Mount    int // index into FusedPlan.Targets
	Err      error
	Panicked bool
}

func (e *MountError) Error() string { return e.Err.Error() }
func (e *MountError) Unwrap() error { return e.Err }

// FanOut runs fn against every target CONCURRENTLY and returns the failure of
// the LOWEST-INDEXED target that failed, or nil when all succeeded.
//
// It is the one concurrent lens fan-out, and the rules it keeps are the ones
// fact b9c7ab96 states:
//
//   - fn MUST confine its writes to its own index; the caller reduces the
//     per-index slots afterwards, in index order, on its own goroutine. A
//     mount's INDEX is its identity to FuseRRF and WriteFirstWinners, so
//     collecting results as they arrive would make the response depend on
//     scheduling.
//   - the reported failure is the lowest index (what a serial loop would have
//     hit first), by index and not by failure kind: a high-index panic does
//     not outrank a low-index ordinary error.
//   - a panic in fn becomes that target's error. net/http recovers panics on
//     the REQUEST goroutine only, so an unrecovered panic in a bare fan-out
//     goroutine would take the whole server down.
//   - the mount's label is built BEFORE the goroutine and without dereferencing
//     anything that may be absent: a recover handler that can itself panic (a
//     nil RepoInstance's Name()) is worse than none.
func FanOut(targets []Target, fn func(i int, t Target) error) *MountError {
	failures := make([]*MountError, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		label := "mount " + t.RT.Branch
		if t.RT.RI != nil {
			label = "mount " + t.RT.RI.Name() + "@" + t.RT.Branch
		}
		wg.Add(1)
		go func(i int, t Target, label string) {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					failures[i] = &MountError{Mount: i, Panicked: true,
						Err: fmt.Errorf("%s panicked: %v", label, p)}
				}
			}()
			if err := fn(i, t); err != nil {
				failures[i] = &MountError{Mount: i, Err: err}
			}
		}(i, t, label)
	}
	wg.Wait()
	for _, f := range failures {
		if f != nil {
			return f
		}
	}
	return nil
}

// fanOutSerial runs fn against every target in order on the caller's goroutine
// and stops at the first failure (which is then also the lowest index). A panic
// is NOT recovered: it propagates to the caller exactly as it would from an
// inline loop.
func fanOutSerial(targets []Target, fn func(i int, t Target) error) *MountError {
	for i, t := range targets {
		if err := fn(i, t); err != nil {
			return &MountError{Mount: i, Err: err}
		}
	}
	return nil
}

// FusedOptions tunes how the legs run.
type FusedOptions struct {
	// Serial runs the legs one after another on the caller's goroutine, stopping
	// at the first failure, with no panic recovery. The default is FanOut.
	//
	// It exists only to keep REST /lenses/{lens}/facts byte-identical to what it
	// did before the pipeline was shared: that handler has always read its
	// mounts serially, while /lenses/{lens}/search and MCP knomit_query fan out
	// concurrently. The two differ only when a leg PANICS (serial: the panic
	// reaches the caller; concurrent: a "panicked" MountError). Whether the
	// recency listing should fan out concurrently too is an open decision, not
	// an accident to fix here.
	Serial bool
}

// Hit is one row of a fused read, with the mount it came from.
//
// Mount indexes FusedPlan.Targets (a POSITION, which narrowing shifts);
// Target.Mount is the mount's STABLE index into Binding.Reads(). Target carries
// the mount's RepoInstance (Target.RT.RI: its ID() is the repo identity, its
// Name() the mount name) and the branch the row was read at. The pinned version
// the row came from is the row's own CommitHash / BlobHash.
type Hit[T any] struct {
	Mount  int
	Target Target
	Row    T
}

// Fused is the result of a fused read.
type Fused[T any] struct {
	// Hits is the union in fused order with write-first dedupe applied and
	// NOTHING truncated: paging and caps are the caller's.
	Hits []Hit[T]
	// Lists, Totals and Diags are the legs' raw per-mount answers in binding
	// order (Totals is the leg's own count; 0 for a search leg).
	Lists  [][]T
	Totals []int
	Diags  []index.SearchDiag
}

// ListLens is each mount's raw list length, in binding order.
func (f *Fused[T]) ListLens() []int {
	ns := make([]int, len(f.Lists))
	for i, l := range f.Lists {
		ns[i] = len(l)
	}
	return ns
}

// SearchLeg reads one mount's relevance-ranked list. q is the plan's Opts with
// Path set to the target's repo-relative filter and Diag to the mount's slot.
// t.Mount is the mount's stable identity (its index into Binding.Reads()); a
// leg that reads per-mount state — a frozen anchor, a watermark — keys on it,
// never on the target's position in the plan.
type SearchLeg func(ctx context.Context, t Target, q index.SearchOptions) ([]index.SearchResult, error)

// RecentLeg reads one mount's recency listing and that mount's total count. It
// must return the list committed_at DESC when q.Text is empty (RecentFacts
// returns it relevance-ranked when q.Text is set). t.Mount is the stable
// per-mount identity, as for SearchLeg.
type RecentLeg func(ctx context.Context, t Target, q index.SearchOptions) ([]index.RecentFactEntry, int, error)

// FusedSearch is the relevance read: the legs' ranked lists fused by
// reciprocal rank (FuseRRF), then write-first deduped. A failed leg returns
// *MountError.
func FusedSearch(ctx context.Context, p *FusedPlan, leg SearchLeg, o FusedOptions) (*Fused[index.SearchResult], error) {
	return fused(ctx, p, o, false,
		func(ctx context.Context, t Target, q index.SearchOptions) ([]index.SearchResult, int, error) {
			rows, err := leg(ctx, t, q)
			return rows, 0, err
		},
		func(r index.SearchResult) string { return r.Path },
		func(r index.SearchResult) int64 { return r.CommittedAt })
}

// MaxRecencyDepth is the deepest a text-less recency listing asks any mount
// for (FusedRecentDeep's backstop): past it a pathological overlap gets a
// best-effort answer rather than an unbounded fan-out.
const MaxRecencyDepth = 10000

// FusedRecentDeep is FusedRecent with the merge-horizon DEEPENING of a
// text-less recency listing, shared by REST /lenses/{lens}/facts and the lens
// FactSet so their lists cannot drift apart.
//
// Depth is re-derived, not fixed, because write-first dedupe spends it: a
// duplicate that loses to the write mount still consumed a row of its own
// mount's depth, so a re-rooted fork beside its upstream can spend the whole
// budget on copies while its own newest facts sit just past the depth. The
// bound is the k-way merge horizon: a TRUNCATED mount (fewer rows fetched than
// its own total) hides only rows older than the last row it returned, so every
// merged row at or above the newest such cutoff is correct and complete. While
// fewer than need merged rows are that safe, the listing runs again with the
// depth doubled, until nothing is truncated, the need is met, or maxDepth.
//
// A query WITH text runs once: per-mount relevance ranks do not merge, so
// there is no horizon and no deeper row to fetch. It returns the last round
// and whether any mount was truncated in it (which decides whether the union
// length or the summed per-mount totals is the honest count).
func FusedRecentDeep(ctx context.Context, p *FusedPlan, leg RecentLeg, o FusedOptions, depth, need, maxDepth int) (*Fused[index.RecentFactEntry], bool, error) {
	for {
		p.Opts.Limit = depth
		res, err := FusedRecent(ctx, p, leg, o)
		if err != nil {
			return nil, false, err
		}
		truncated := false
		// The newest commit timestamp below which some truncated mount may
		// still hide a row; zero while nothing is truncated (every row safe).
		horizon := int64(0)
		for i, entries := range res.Lists {
			if len(entries) < res.Totals[i] {
				truncated = true
				if last := entries[len(entries)-1].CommittedAt; last > horizon {
					horizon = last
				}
			}
		}
		if p.Opts.Text != "" || !truncated || depth >= maxDepth {
			return res, truncated, nil
		}
		// The leading run of merged rows no unfetched row could outrank.
		safe := 0
		for _, h := range res.Hits {
			if h.Row.CommittedAt < horizon {
				break
			}
			safe++
		}
		if safe >= need {
			return res, truncated, nil
		}
		depth = min(depth*2, maxDepth)
	}
}

// FusedRecent is the recency listing: a text-less query merges the legs' lists
// by committed_at (MergeRecent over every fetched row); a query WITH text fuses
// them by reciprocal rank, because RecentFacts then ranks by relevance and
// per-mount relevance ranks do not compare. Then write-first dedupe. A failed
// leg returns *MountError.
func FusedRecent(ctx context.Context, p *FusedPlan, leg RecentLeg, o FusedOptions) (*Fused[index.RecentFactEntry], error) {
	return fused(ctx, p, o, true, leg,
		func(e index.RecentFactEntry) string { return e.Path },
		func(e index.RecentFactEntry) int64 { return e.CommittedAt })
}

func fused[T any](
	ctx context.Context, p *FusedPlan, o FusedOptions, recency bool,
	leg func(ctx context.Context, t Target, q index.SearchOptions) ([]T, int, error),
	pathOf func(T) string, stampOf func(T) int64,
) (*Fused[T], error) {
	n := len(p.Targets)
	out := &Fused[T]{Lists: make([][]T, n), Totals: make([]int, n), Diags: make([]index.SearchDiag, n)}
	run := func(i int, t Target) error {
		q := p.Opts
		q.Path = t.Path
		q.Diag = &out.Diags[i]
		rows, total, err := leg(ctx, t, q)
		if err != nil {
			return err
		}
		out.Lists[i] = rows
		out.Totals[i] = total
		return nil
	}
	var f *MountError
	if o.Serial {
		f = fanOutSerial(p.Targets, run)
	} else {
		f = FanOut(p.Targets, run)
	}
	if f != nil {
		return nil, f
	}

	var order []MountRef
	if recency && p.Opts.Text == "" {
		stamps := make([][]int64, n)
		fetched := 0
		for i, list := range out.Lists {
			stamps[i] = make([]int64, len(list))
			for j, e := range list {
				stamps[i][j] = stampOf(e)
			}
			fetched += len(list)
		}
		order = MergeRecent(stamps, fetched)
	} else {
		order = FuseRRF(out.ListLens())
	}

	winner := WriteFirstWinners(p.Targets, p.Binding.Write(), out.Lists, pathOf)
	out.Hits = make([]Hit[T], 0, len(winner))
	for _, ref := range order {
		row := out.Lists[ref.Mount][ref.Rank]
		if winner[pathOf(row)] != ref.Mount {
			continue
		}
		out.Hits = append(out.Hits, Hit[T]{Mount: ref.Mount, Target: p.Targets[ref.Mount], Row: row})
	}
	return out, nil
}
