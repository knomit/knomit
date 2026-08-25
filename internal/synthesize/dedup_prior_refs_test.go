package synthesize

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// fixedPairSearch is a SearchQuery whose Search returns a canned result set,
// so a dedup pair can be forced without standing up an embedder. Every other
// method — crucially FactExistsAt, which the ref gate resolves through — is
// the real one.
type fixedPairSearch struct {
	SearchQuery
	results []store.SearchResult
}

func (f *fixedPairSearch) Search(context.Context, string, store.SearchOptions) ([]store.SearchResult, error) {
	return f.results, nil
}

// searchHit is a result above any dedup threshold — dedupCluster divides Score
// by 100 to get the cosine it compares against the threshold.
func searchHit(path string) store.SearchResult {
	return store.SearchResult{
		FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: path}},
		Score:        96,
	}
}

// TestDedupCluster_KeepsUnresolvableCarriedRefs regresses knomit#103: a dedup
// merge grafts the loser's refs onto the winner and used to hand the whole
// union to the ref gate with only the loser's path as prior, re-litigating
// citations both facts had carried for months against today's live index. One
// stale citation anywhere in one cluster then aborted the entire review
// session — every pass in the run lost, not just the affected merge — and it
// aborted again on every retry, so the corpus could never be reviewed until
// the facts were hand-edited.
//
// internal/refs' contract: "A ref is checked ONCE, against the commit the write
// lands on. Refs a fact ALREADY carried are never re-checked." A mechanical
// merge introduces no new citation, so nothing in the union is new.
//
// The seeds go in through the store rather than a write path, which is not a
// test shortcut: it is how a corpus acquires facts whose refs no longer
// resolve — written before the gate existed, or their target retracted past
// the walk-back horizon, or the citing repo's history rewritten. The gate's
// `prior` parameter exists precisely because this state is real and must stay
// editable.
func TestDedupCluster_KeepsUnresolvableCarriedRefs(t *testing.T) {
	ctx := context.Background()
	svc, branch := newSourcesTestRepo(t)

	const (
		winnerPath  = "kb/technology/winner.md"
		loserPath   = "kb/technology/loser.md"
		winnerStale = "kb/technology/winner-cited-gone.md"
		loserStale  = "kb/technology/loser-cited-gone.md"
		liveRefPath = "kb/technology/still-here.md"
	)

	// Precondition: the confidences that pick the winner must actually differ,
	// or "the winner survived" is an assertion about a coin toss.
	const winnerConf, loserConf = 0.9, 0.5
	require.NotEqual(t, winnerConf, loserConf)

	// A real, resolvable citation alongside the stale ones, so the assertion
	// below distinguishes "refs survived" from "ref list was emptied".
	seedOriginFact(t, svc, branch, liveRefPath, fact.Observation, fact.Authored, 0.7, 1, nil)
	seedOriginFact(t, svc, branch, winnerPath, fact.Observation, fact.Authored, winnerConf, 1,
		[]string{winnerStale, liveRefPath})
	seedOriginFact(t, svc, branch, loserPath, fact.Observation, fact.Authored, loserConf, 1,
		[]string{loserStale})

	cluster := []factForLLM{
		{File: winnerPath, Title: "winner", Body: "b", Type: string(fact.Observation), Confidence: winnerConf, Sources: 1},
		{File: loserPath, Title: "loser", Body: "b", Type: string(fact.Observation), Confidence: loserConf, Sources: 1},
	}
	idx := &fixedPairSearch{
		SearchQuery: svc.Search(),
		results: []store.SearchResult{
			searchHit(winnerPath),
			searchHit(loserPath),
		},
	}

	surviving, err := dedupCluster(ctx, cluster, svc.Facts(), idx, 0.92, "test",
		func(ProgressEvent) {}, branch, bareRefFixture)
	require.NoError(t, err, "a carried ref that no longer resolves must not abort the dedup pass")

	require.Len(t, surviving, 1)
	require.Equal(t, winnerPath, surviving[0].File)

	rf, err := svc.Facts().ReadFact(ctx, branch, winnerPath, nil)
	require.NoError(t, err)
	merged, err := fact.ParseFact(winnerPath, rf.Content)
	require.NoError(t, err)

	// Provenance survives: dropping the unresolvable refs to satisfy a check
	// that should never have run on them destroys the lineage the merge exists
	// to preserve.
	require.Contains(t, merged.Refs, winnerStale, "winner's own carried ref was dropped")
	require.Contains(t, merged.Refs, loserStale, "loser's carried ref was not grafted onto the winner")
	require.Contains(t, merged.Refs, liveRefPath)
	require.Contains(t, merged.Refs, loserPath, "the merge must cite the fact it subsumed")
}

// TestDedupMergeRefs_CarriedIsAnIndependentSnapshot guards 0ee925f4: passing
// the same slice as both `refs` and `prior` to refs.Gate.Apply exempts every
// local ref in the write, unconditionally, and the call still compiles, still
// canonicalizes, and still reads like a gate. Because this merge's write list
// happens to be entirely carried today, the aliased form would be correct FOR
// TODAY'S CODE — and would launder the first ref a later change appends here.
//
// So the property under test is not "prior equals the write list" (it does),
// it is "prior was built from the operands and can diverge from the write
// list". This test binds the RUNTIME half — the two lists are distinct
// objects — and reddens when the helper returns one slice for both. The
// construction half, which no runtime assertion can see because a copy and a
// derivation are indistinguishable at every input, is bound structurally by
// TestDedupMergeRefs_CarriedIsBuiltOnlyFromTheOperands below.
//
// The gate calls at the end are CONTROLS on the gate, not sabotage targets:
// no edit to the helper can redden them. They record what the exemption does
// and does not cover — a carried ref that no longer resolves passes, a ref
// that was never carried is still rejected — which is the behaviour the two
// arguments are meant to produce.
func TestDedupMergeRefs_CarriedIsAnIndependentSnapshot(t *testing.T) {
	ctx := context.Background()
	svc, branch := newSourcesTestRepo(t)

	const (
		winnerPath = "kb/technology/winner.md"
		loserPath  = "kb/technology/loser.md"
		staleRef   = "kb/technology/cited-gone.md"
		liveRef    = "kb/technology/still-here.md"
	)
	seedOriginFact(t, svc, branch, liveRef, fact.Observation, fact.Authored, 0.7, 1, nil)
	seedOriginFact(t, svc, branch, loserPath, fact.Observation, fact.Authored, 0.5, 1, nil)

	winnerRefs := []string{staleRef}
	loserRefs := []string{liveRef}
	// Precondition: the operands must carry DIFFERENT refs, or a union that
	// dropped one of them would still look right.
	require.NotEqual(t, winnerRefs, loserRefs)

	write, carried := dedupMergeRefs(winnerRefs, loserRefs, loserPath)

	// The merge's own behaviour is unchanged: union of both, plus the loser.
	require.ElementsMatch(t, []string{staleRef, liveRef, loserPath}, write)

	// Structurally satisfied today — nothing in the write list is new — which
	// is what keeps a carried-but-unresolvable ref out of the gate's way.
	require.ElementsMatch(t, write, carried)

	// ...but not by aliasing. Same backing array means `prior` can never
	// differ from `refs`, whatever a later change does to the write list.
	require.NotSame(t, &write[0], &carried[0],
		"prior must be its own list, not the write list under a second name")

	gate := refs.New(bareRefFixture, refs.FromFactQuery(svc.Search(), branch))

	// Positive control: the carried set, unresolvable member and all, passes.
	// This is the #103 case and it must not be rejected.
	_, _, err := gate.Apply(ctx, winnerPath, write, carried)
	require.NoError(t, err, "a ref both operands carried must not be re-judged")

	// The other half of the control: the exemption is not blanket. A ref the
	// merge did not carry, of the kind a later change might append here (a
	// lineage pointer to a synthesized parent), is still gated.
	const ghost = "kb/technology/never-existed.md"
	extended := append(append([]string(nil), write...), ghost)
	_, _, err = gate.Apply(ctx, winnerPath, extended, carried)
	require.Error(t, err, "a ref that was never carried must still be rejected")
	require.Contains(t, err.Error(), ghost)
}

// TestDedupMergeRefs_CarriedIsBuiltOnlyFromTheOperands binds the property the
// helper exists for, which is a property of its CONSTRUCTION and therefore
// invisible to any runtime assertion: the carried set is derived from the
// operands, never from the list being written.
//
// The rejected shape produces a `carried` element-for-element equal to the
// write list, in its own array, at every possible input:
//
//	carried = append([]string(nil), write...)   // a copy of the write list
//
// It is wrong for the reason 0ee925f4 gives: a snapshot of the write list is
// only as good as its position, so a later change appending a ref above it is
// exempted from the gate and the same change below it is checked. Deriving
// from the operands makes divergence hold wherever the append lands. The
// difference is real and unobservable, which is when a source-level check is
// the honest instrument — the MN4/MN6 pattern, applied to one function.
//
// This is a WHITELIST, and it has to be. An earlier version blacklisted the
// identifier `write`, which left the property one refactor wide: hoist the
// write-list construction into a local `w` — to log it, to assert on it, to
// reuse it — build the carried set from `w`, and the helper is the rejected
// positional-copy shape under a name the check does not know. That version
// also reddened a pure rename of the results, with a message naming an
// identifier that no longer existed. Reading the declaration fixes both: the
// operands are whatever the parameters are called, and anything else an
// assignment reaches for is a local the carried set must not be built from.
func TestDedupMergeRefs_CarriedIsBuiltOnlyFromTheOperands(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dedup.go", nil, 0)
	require.NoError(t, err)

	const fn = "dedupMergeRefs"

	var decl *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if f, ok := n.(*ast.FuncDecl); ok && f.Name.Name == fn {
			decl = f
		}
		return decl == nil
	})
	require.NotNil(t, decl, "%s must exist for this check to mean anything", fn)

	// The operands, as the function itself declares them: a rename must follow
	// automatically rather than redden correct code.
	allowed := map[string]bool{}
	for _, f := range decl.Type.Params.List {
		for _, n := range f.Names {
			allowed[n.Name] = true
		}
	}
	require.Greater(t, len(allowed), 0, "%s must take the operands as parameters", fn)

	require.NotNil(t, decl.Type.Results, "%s must declare named results", fn)
	var results []string
	for _, f := range decl.Type.Results.List {
		for _, n := range f.Names {
			results = append(results, n.Name)
		}
	}
	require.Len(t, results, 2, "%s returns the write list and the carried set, in that order", fn)
	carried := results[1]
	// The carried set may be built up from itself. The write list is
	// deliberately NOT whitelisted: it must not appear here at all.
	allowed[carried] = true

	// Package qualifiers, read from this file's own imports, plus the
	// predeclared identifiers a construction legitimately uses.
	for _, imp := range file.Imports {
		name := strings.Trim(imp.Path.Value, `"`)
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		allowed[name] = true
	}
	for _, b := range []string{"append", "make", "copy", "len", "cap", "nil", "string", "new"} {
		allowed[b] = true
	}

	assignments := 0
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		assigns := false
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == carried {
				assigns = true
			}
		}
		if !assigns {
			return true
		}
		assignments++
		var check func(ast.Node) bool
		check = func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				// The member name (fact.UnionStrings) is not an identifier this
				// check has an opinion about. The qualifier is, so walk only it.
				ast.Inspect(v.X, check)
				return false
			case *ast.Ident:
				if allowed[v.Name] {
					return true
				}
				t.Fatalf("%s builds %s from %q at %s, which is neither an operand nor %s "+
					"itself. The carried set must be derived from the parameters, where "+
					"'this was already carried' is provably true — never from the list being "+
					"written, nor from a local standing in for it. A snapshot of the write "+
					"list is only as good as the line it sits on: a later change appending a "+
					"ref above it is silently exempted from the gate, one below it is checked "+
					"(0ee925f4).",
					fn, carried, v.Name, fset.Position(v.Pos()), carried)
				return false
			}
			return true
		}
		for _, rhs := range as.Rhs {
			ast.Inspect(rhs, check)
		}
		return true
	})

	// A body that never assigns the carried result returns the zero value or
	// the write list under its own name — the aliasing shape — and would
	// otherwise pass a check that only inspects assignments.
	require.Greater(t, assignments, 0,
		"%s must build %s explicitly, or there is no snapshot to speak of", fn, carried)
}

// TestFactForLLM_CarriesNoRefs pins the premise the annex §6.2 correction
// rests on. #103 named decision.go's merge and distill gate calls as the
// dedup sites that omit `prior`; they are not — they write NEW facts whose
// refs are wholly LLM-authored, so there is nothing carried to exempt and
// `prior: nil` is correct. That verdict holds only because the model never
// sees the cluster members' citations: factForLLM is the whole of what those
// paths show it, and it has no Refs field.
//
// Add one and both sites become wrong the moment the model starts copying a
// member's refs into its output — the #103 failure, in the two places #103
// wrongly accused, with nothing to announce the change. Reading the three
// sites and agreeing is still a reading; this is the tripwire, and it names
// where to look when it fires.
func TestFactForLLM_CarriesNoRefs(t *testing.T) {
	typ := reflect.TypeOf(factForLLM{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.ToLower(f.Name)
		tag := strings.ToLower(f.Tag.Get("json"))
		require.NotEqualf(t, "refs", name,
			"factForLLM.%s exposes carried refs to the LLM. decision.go's merge (:214) and "+
				"distill (:324) gate calls pass prior: nil BECAUSE the model cannot copy a "+
				"member's citations into its output — see the gate annex §6.2 correction and "+
				"knomit#103. If this field is intended, those two call sites must be revisited "+
				"in the same change.", f.Name)
		require.NotEqualf(t, "refs", strings.Split(tag, ",")[0],
			"factForLLM.%s is serialized to the model as %q — same consequence as a Refs "+
				"field, see above.", f.Name, tag)
	}
	// Preconditions: the walk must actually be inspecting a populated struct,
	// or an empty loop passes as a guarantee.
	require.Greater(t, typ.NumField(), 5)
	require.NotEqual(t, -1, func() int {
		f, ok := typ.FieldByName("File")
		if !ok {
			return -1
		}
		return f.Index[0]
	}(), "factForLLM must still be the struct this guards")
}
