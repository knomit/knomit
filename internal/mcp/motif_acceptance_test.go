package mcp

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// TestAcceptance_MotifInstructions is Phase 1's SECOND acceptance item against
// a real corpus: the served instructions must be the same bytes whether the
// corpus is empty or full of motifs.
//
// The unit test next door proves this on a fixture. This proves it on real
// data, where a templating mistake would have real vocabulary available to
// leak — and it names every motif the corpus actually holds, so the log says
// whether the check had anything to leak in the first place. A pass on a corpus
// carrying zero motifs is a pass that proved nothing, and the output makes that
// visible rather than hiding it behind a green tick.
//
// The first item — a subject-restating motif stores without it — is measured by
// TestAcceptance_MotifField in internal/synthesize.
//
//	cp ~/.knomit/repos/<uid>.db /tmp/accept.db
//	KNOMIT_PHASE1_DB=/tmp/accept.db go test ./internal/mcp/ \
//	    -run TestAcceptance_MotifInstructions -v
//
// Work on a COPY. The run migrates the schema.
func TestAcceptance_MotifInstructions(t *testing.T) {
	dbPath := os.Getenv("KNOMIT_PHASE1_DB")
	if dbPath == "" {
		t.Skip("set KNOMIT_PHASE1_DB to a COPY of a repo database to run the acceptance measurement")
	}
	ctx := context.Background()

	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())

	branch := os.Getenv("KNOMIT_PHASE1_BRANCH")
	if branch == "" {
		branch, err = svc.Branches().DefaultBranch(ctx)
		require.NoError(t, err)
	}

	motifs, err := corpusMotifs(ctx, svc, branch)
	require.NoError(t, err)
	t.Logf("corpus %q carries %d distinct motif strings", branch, len(motifs))
	if len(motifs) > 0 {
		show := motifs
		if len(show) > 25 {
			show = show[:25]
		}
		t.Logf("sample: %s", strings.Join(show, ", "))
	} else {
		t.Log("NOTE: this corpus carries no motifs of its own yet — expected " +
			"before anything has written one. The planted sentinel below is " +
			"what keeps the leak check from passing vacuously.")
	}

	// Plant a sentinel so the leak check is REAL regardless of corpus state.
	// Without it, a corpus that carries no motifs yet — which every corpus does
	// until an agent writes one — makes this test a green tick over an empty
	// loop. The sentinel is written through the ordinary write path into the
	// COPY, so it is a genuine corpus motif by the time it is checked.
	const sentinel = "sentinel-leak-canary"
	plantMotif(t, svc, branch, sentinel)
	motifs = append(motifs, sentinel)

	replanted, err := corpusMotifs(ctx, svc, branch)
	require.NoError(t, err)
	require.Contains(t, replanted, sentinel,
		"the sentinel must actually be in the corpus, or it proves nothing")

	// The LEAK check is what this harness adds over the unit test.
	//
	// Byte-identity across corpora is proven structurally: the corpus is not an
	// input to ProfileInstructions at all, and TestMN1_InstructionsAreCorpusIndependent
	// builds both sides from real stores and compares bytes. Re-staging that
	// here would be a comparison of a value with itself. What only a real
	// corpus can test is whether any of the strings it ACTUALLY holds appear in
	// what a session is served — a templating mistake needs real vocabulary
	// available before it can leak any.
	for _, profile := range []string{"code", "chat", "generic"} {
		t.Run(profile, func(t *testing.T) {
			served := ProfileInstructions(profile, "kb", fact.CodeOntology())
			require.Contains(t, served, "### Motifs")
			for _, m := range motifs {
				require.NotContainsf(t, served, m,
					"MN1: the corpus motif %q leaked into the %s instructions", m, profile)
			}
			t.Logf("%s: %d bytes, leaks none of the %d corpus motifs",
				profile, len(served), len(motifs))
		})
	}
}

// plantMotif writes one fact carrying motif into the copied corpus, so the leak
// check has something to leak on a corpus that has none of its own yet.
func plantMotif(t *testing.T, svc *store.Service, branch, motif string) {
	t.Helper()
	f := fact.NewFact("kb/meta/acceptance/motif-sentinel.md")
	f.Title = "Acceptance sentinel"
	f.Body = "Planted by TestAcceptance_MotifInstructions. Safe to delete."
	f.Type = fact.Observation
	f.Domain = []string{"meta"}
	f.Entities = []string{}
	f.Refs = []string{}
	f.Confidence = 0.5
	f.Sources = 1
	f.Motifs = []string{motif}
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body,
		"acceptance: plant motif sentinel", "test")
	require.NoError(t, err)
}

// corpusMotifs returns every distinct motif string live on branch, read from
// the committed files rather than the junction so it reports what is actually
// on disk.
func corpusMotifs(ctx context.Context, svc *store.Service, branch string) ([]string, error) {
	paths, err := svc.Facts().ListAll(ctx, branch)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		res, rerr := svc.Facts().ReadFact(ctx, branch, p, nil)
		if rerr != nil {
			continue
		}
		f, perr := fact.ParseFact(p, res.Content)
		if perr != nil {
			continue // not a fact file
		}
		for _, m := range f.Motifs {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
