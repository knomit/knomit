package synthesize

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// TestAcceptance_MotifField runs Phase 1's FIRST acceptance item against a
// REAL knomit repo database, and prints what the field does to a real corpus:
// a fact written with a subject-restating motif stores WITHOUT it.
//
// The second item — server instructions byte-identical regardless of corpus
// state — is measured by TestAcceptance_MotifInstructions in internal/mcp.
// It cannot live here: internal/mcp imports this package, so the dependency
// cannot run back the other way.
//
// Skipped unless KNOMIT_PHASE1_DB names a COPY of a repo DB, matching the
// Phase-0 harness next door: it needs a real corpus, and it is a MEASUREMENT
// rather than a contract. The assertions are deliberately weak — the strip must
// not error, must not take everything, and must not take nothing. What makes it
// worth keeping is the output: which real facts' own subject words would have
// eaten a motif, and which survived.
//
//	cp ~/.knomit/repos/<uid>.db /tmp/accept.db
//	KNOMIT_PHASE1_DB=/tmp/accept.db go test ./internal/synthesize/ \
//	    -run TestAcceptance_MotifField -v -timeout 30m
//
// Work on a COPY. The run migrates the schema.
//
// No embedder is configured: unlike Phase 0, nothing here embeds anything. That
// is itself worth stating — a motif is a string and a junction row, and the
// design has NO per-fact-motif embeddings anywhere.
func TestAcceptance_MotifField(t *testing.T) {
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

	acceptanceSubjectStrip(t, svc, branch)
}

// acceptanceSubjectStrip walks real facts, synthesizes a subject motif for each
// out of the fact's OWN entity/domain/path words, and checks the strip removes
// it — then does the same with a generic regularity name and checks it survives.
//
// The generator is the interesting half. It builds the motif a careless author
// would actually write: two of the fact's own subject words, kebab-joined. If
// the strip is too narrow that motif survives; if it is too broad the generic
// control dies. Both directions are measured and logged.
func acceptanceSubjectStrip(t *testing.T, svc *store.Service, branch string) {
	ctx := context.Background()
	paths, err := svc.Facts().ListAll(ctx, branch)
	require.NoError(t, err)
	require.NotEmpty(t, paths, "the corpus must have facts, or this measures nothing")

	// A control that is a real regularity name and shares no word with any
	// plausible subject. It must survive on every fact.
	const control = "zero-value-as-valid"

	var examined, stripped, notStripped, controlSurvived, skipped int
	var strippedEg, survivedEg []string

	for _, p := range paths {
		// No suffix pre-filter: the ParseFact below already rejects every
		// non-fact file, and a second rule for "what is a fact file" is a
		// second rule to keep in sync (ref_consumers_test.go polices exactly
		// this).
		res, rerr := svc.Facts().ReadFact(ctx, branch, p, nil)
		if rerr != nil {
			skipped++
			continue
		}
		f, perr := fact.ParseFact(p, res.Content)
		if perr != nil {
			skipped++ // not a fact file (README, ontology, ...)
			continue
		}

		subject := subjectMotifFor(f)
		if subject == "" {
			skipped++ // too few usable subject words to build a two-word motif
			continue
		}
		examined++

		f.Motifs = []string{subject, control}
		out, serr := fact.SerializeFact(f)
		require.NoErrorf(t, serr, "the strip must never error, and did on %s", p)

		back, berr := fact.ParseFact(p, out)
		require.NoError(t, berr)

		hasSubject := containsStr(back.Motifs, subject)
		if hasSubject {
			notStripped++
			if len(survivedEg) < 8 {
				survivedEg = append(survivedEg, fmt.Sprintf("%s\n      motif %q survived; entities=%v domain=%v",
					p, subject, f.Entities, f.Domain))
			}
		} else {
			stripped++
			if len(strippedEg) < 8 {
				strippedEg = append(strippedEg, fmt.Sprintf("%s\n      motif %q stripped", p, subject))
			}
		}
		if containsStr(back.Motifs, control) {
			controlSurvived++
		}
	}

	t.Logf("\n=== subject strip on a real corpus ===\n"+
		"  facts examined:            %d  (skipped %d: unparseable or too few subject words)\n"+
		"  subject motif stripped:    %d  (%.1f%%)\n"+
		"  subject motif survived:    %d  (%.1f%%)\n"+
		"  control %q survived: %d/%d\n",
		examined, skipped,
		stripped, pct(stripped, examined),
		notStripped, pct(notStripped, examined),
		control, controlSurvived, examined)

	if len(strippedEg) > 0 {
		t.Logf("stripped examples:\n    - %s", strings.Join(strippedEg, "\n    - "))
	}
	if len(survivedEg) > 0 {
		t.Logf("SURVIVED (inspect these — each is a subject motif the strip let through):\n    - %s",
			strings.Join(survivedEg, "\n    - "))
	}

	require.Positive(t, examined, "no fact yielded a testable subject motif; the generator is wrong, not the strip")
	// Weak by design. A strip that removed nothing is broken; one that removed
	// the control on any fact is over-broad. Between those, the RATE is a
	// measurement to read, not a threshold to enforce — it depends entirely on
	// how this corpus names things.
	require.Positive(t, stripped, "the strip removed nothing on a real corpus")
	require.Equal(t, examined, controlSurvived,
		"a generic regularity name must survive on every fact; the strip is over-broad")
}

// subjectMotifFor builds the motif a careless author would write for f: two of
// the fact's own subject words, kebab-joined. Returns "" when the fact has
// fewer than two usable words.
func subjectMotifFor(f fact.Fact) string {
	var words []string
	seen := map[string]bool{}
	for _, s := range append(append([]string{}, f.Entities...), f.Domain...) {
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return r < 'a' || r > 'z'
		}) {
			if len(w) < 3 || seen[w] {
				continue
			}
			seen[w] = true
			words = append(words, w)
		}
	}
	if len(words) < 2 {
		return ""
	}
	return words[0] + "-" + words[1]
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}
