package synthesize

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// The seed scan has TWO paths to a fact.Fact — the full scan projects a search
// hit (factFromSearchResult), the incremental scan parses the file
// (fact.ParseFact) — and factFromSearchResult's own doc comment promises they
// "yield the same type".
//
// This asserts that promise GENERICALLY rather than field by field. The defect
// it was written for dropped Motifs, but a motif-shaped assertion would not
// have caught the next field to go missing, and the bug is not "motifs were
// forgotten" — it is "two projections of one thing were allowed to disagree".
//
// The consequence when they do disagree is invisible: a full-scan session (any
// FIRST session on a corpus) hands prune, distill and bridging a different fact
// from the one an incremental session hands them, and every test that happens
// to run one path passes.
func TestSeedScanPaths_ProduceTheSameFact(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)

	const path = "kb/gotchas/uitesting/agentclicks.md"
	f := fact.NewFact(path)
	f.Title = "An agent testing a UI will execute JavaScript instead of clicking"
	f.Body = "Driving app state directly bypasses the path the verifier believes it is checking."
	f.Type = fact.Observation
	f.Kind = fact.Epistemic
	f.Domain = []string{"evaluation", "coding-agents"}
	f.Entities = []string{"Cognition", "Devin"}
	f.Motifs = []string{"measure-becomes-target"}
	f.Confidence = 0.8
	f.Sources = 1
	f.EvidenceWeight = 2.5
	f.Refs = []string{"kb/gotchas/uitesting/other.md"}
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(ctx, env.branch, path, content, "write", "test")
	require.NoError(t, err)

	// Path 1 — the full scan's projection of a search hit.
	results, err := env.svc.Search().Search(ctx, env.branch, store.SearchOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, results, 1)
	fromSearch := factFromSearchResult(results[0])

	// Path 2 — the incremental scan's parse of the same bytes.
	read, err := env.svc.Facts().ReadFact(ctx, env.branch, path, nil)
	require.NoError(t, err)
	fromParse, err := fact.ParseFact(path, read.Content)
	require.NoError(t, err)

	// GENERIC, by reflection over fact.Fact's exported fields.
	//
	// The first version of this test hand-enumerated the fields, under a
	// comment claiming it was generic — and the review proved the claim false
	// by finding EvidenceWeight divergent at HEAD and absent from the list.
	// A field added to fact.Fact and forgotten in one projection now fails here
	// without anyone remembering to add a line.
	//
	// Two divergences are DECLARED rather than compared, each with its reason;
	// everything else must match exactly.
	declared := map[string]string{
		// ParseFact applies the type-aware default on read; the projection
		// carries the elided value. Harmless at HEAD — every consumer tests for
		// Discovered specifically, and SerializeFact never elides that — and
		// owned by the origin contract, not by this phase (review M1).
		"Origin": "parse defaults it, the projection carries the elided value",
	}

	pv := reflect.ValueOf(fromParse)
	sv := reflect.ValueOf(fromSearch)
	typ := pv.Type()
	compared := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		if why, ok := declared[f.Name]; ok {
			t.Logf("declared divergence on %s: %s", f.Name, why)
			continue
		}
		// PARSE DIAGNOSTICS are not fact data: RefWarnings and MotifWarnings
		// record what the lenient parser DISCARDED, they are never persisted,
		// and no projection can carry them. They are asserted EMPTY on both
		// sides rather than skipped — a clean fixture that starts producing
		// warnings is a finding, and skipping them by name would hide it.
		if strings.HasSuffix(f.Name, "Warnings") {
			require.Emptyf(t, pv.Field(i).Interface(), "clean fixture produced %s", f.Name)
			require.Emptyf(t, sv.Field(i).Interface(), "projection produced %s", f.Name)
			continue
		}
		// PRECONDITION (lesson 5): the fixture must give this field a
		// non-zero value, or "equal" is two zero values agreeing and the
		// comparison proves nothing. EvidenceWeight was divergent AND unset by
		// the old fixture — both halves of the miss.
		require.Falsef(t, pv.Field(i).IsZero(),
			"fixture must set %s to a non-zero value, or comparing it tests nothing "+
				"(add it to the fixture above, do NOT exempt it here)", f.Name)
		require.Equalf(t, pv.Field(i).Interface(), sv.Field(i).Interface(),
			"scan paths disagree about %s", f.Name)
		compared++
	}
	require.Positive(t, compared, "a parity test that compared no fields is not a test")

	// The unexported path is not reachable by reflection; assert it directly.
	require.Equal(t, fromParse.Path(), fromSearch.Path())

	// And the declared divergence is asserted rather than merely excused, so it
	// fails loudly the day someone fixes it and forgets this list.
	require.Equal(t, fact.Authored, fromParse.Origin, "the parse path defaults it")
	require.Empty(t, string(fromSearch.Origin), "the projection carries the elided value")
}
