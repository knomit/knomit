package textnorm

import (
	"strings"
	"sync"
	"testing"
	"unicode"
	"unsafe"

	"github.com/gertd/go-pluralize"
	"github.com/stretchr/testify/require"
)

// resetStemMemo empties the memo so a test starts from a known state. Tests
// that read the memo must not race with a neighbour populating it, so they run
// serially — none of them calls t.Parallel().
func resetStemMemo() {
	stemMemo.Range(func(k, _ any) bool {
		stemMemo.Delete(k)
		return true
	})
}

func stemMemoEntries() map[string]string {
	out := map[string]string{}
	stemMemo.Range(func(k, v any) bool {
		out[k.(string)] = v.(string)
		return true
	})
	return out
}

// unmemoizedStem is Stem's definition with the memo removed — the reference
// the memoized implementation must match byte for byte. Written out rather
// than reached for, so that editing Stem's GUARDS without editing this one
// makes the transparency test fail loudly instead of comparing a function to
// itself.
func unmemoizedStem(t string) string {
	if len(t) <= 3 || strings.HasSuffix(t, "ics") {
		return t
	}
	return pluralize.NewClient().Singular(t)
}

// motifCorpus is real motif spellings from a knomit corpus — the population
// that actually reaches Stem through groupingKey, hyphenated and multi-token,
// with the plurals ("reads", "shifts", "meanings", "ids", "handlers",
// "cases", "lifetimes", "referents") that make singularization observable.
var motifCorpus = []string{
	"intervention-perturbs-measurement", "fix-inherits-its-origin",
	"duplicate-reads-as-corroboration", "name-implies-absent-capability",
	"artifact-mistaken-for-signal", "inherited-context-silently-dropped",
	"remediation-inherits-observation-scope", "implicit-retyping-on-parse",
	"allowlist-avoids-naming-forbidden", "limit-applied-before-filter",
	"failure-presents-as-success", "fixture-cannot-discriminate",
	"proxy-inverts-predicate", "layout-shifts-under-cursor",
	"same-word-two-meanings", "silent-no-op",
	"several-ids-different-lifetimes", "one-name-many-referents",
	"cost-in-wrong-currency", "reentrant-use-exhausts-pool",
	"continuity-mistaken-for-identity", "transient-state-becomes-permanent",
	"partial-capture-wrong-restore", "silent-overwrite-on-collision",
	"derived-not-assumed", "separate-axis-not-enum",
	"signal-cannot-distinguish-cases", "idempotent-absorption",
	"marked-done-blocks-retry", "two-handlers-one-event",
	"shared-path-prevents-divergence", "per-call-full-rescan",
	"exclusion-by-deletion", "aggregation-erases-provenance",
	"edge-trigger-starves-level", "test-mode-hides-condition",
	"single-writer-per-partition", "sentinel-vs-explicit-default",
	"removal-manufactures-dead-code", "mechanical-key-underdetermines-identity",
	// The irregulars a naive stemmer breaks, and the guarded shapes.
	"analyses-of-indices", "matrices-and-theses", "vulnerabilities-in-metrics",
	"aws-tls-llm", "robotics-and-ethics",
}

// THE MEMO IS SEMANTICALLY TRANSPARENT: for every spelling the corpus can
// produce, the memoized pipeline answers exactly what the unmemoized one does.
//
// This is the test that matters. Tokens feeds store.groupingKey, the motif-term
// query tiers and the motif subject-word strip, so a memo that answered
// differently for even one token would silently re-key clusters — a spelling
// would join, or stop joining, a different cluster. Comparing whole
// Tokens(Canonicalize(...)) output, not just Stem, is what covers that: the
// memo sits under Tokens and a per-token divergence shows up in the slice.
func TestStemMemoIsSemanticallyTransparent(t *testing.T) {
	resetStemMemo()

	for _, m := range motifCorpus {
		canonical := Canonicalize(m)

		var want []string
		seen := map[string]bool{}
		for _, f := range strings.FieldsFunc(canonical, func(r rune) bool {
			return r == '/' || unicode.IsSpace(r)
		}) {
			st := unmemoizedStem(f)
			if seen[st] {
				continue
			}
			seen[st] = true
			want = append(want, st)
		}

		// Twice: the cold call populates the memo, the warm call reads it, and
		// both must equal the reference.
		require.Equalf(t, want, Tokens(canonical), "cold pipeline for %q", m)
		require.Equalf(t, want, Tokens(canonical), "warm pipeline for %q", m)
	}
}

// The memo is keyed on the INPUT token and holds the singular form. Keying it
// on the output instead would still return the right answer on every call —
// the first one computes it and stemming is idempotent — so the only thing
// that catches it is asserting which key carries which value.
func TestStemMemoKeysTheInputAndHoldsTheSingular(t *testing.T) {
	resetStemMemo()

	require.Equal(t, "vulnerability", Stem("vulnerabilities"))

	require.Equal(t, map[string]string{"vulnerabilities": "vulnerability"}, stemMemoEntries(),
		"one entry, keyed on the token that was asked about")
}

// The two guards return before the pluralizer runs, so there is nothing to
// memoize: caching them would spend a map entry to avoid a length check.
func TestStemMemoSkipsTheGuardedTokens(t *testing.T) {
	resetStemMemo()

	require.Equal(t, "aws", Stem("aws"))         // len <= 3
	require.Equal(t, "metrics", Stem("metrics")) // -ics

	require.Empty(t, stemMemoEntries())
}

// A stored entry must retain ONLY its own bytes — BOTH HALVES OF IT.
//
// Tokens hands Stem a strings.FieldsFunc substring, which shares the
// canonicalized input's backing array, so an entry stored as-is pins that whole
// string — and request-supplied ?domain= and ?motifs= values do reach here, so
// the pinned buffer can be as long as a caller cares to make it. Comparing the
// data pointers is the only assertion that sees this: the strings are EQUAL
// either way, and an equality test passes on the aliasing version.
//
// The two halves need DIFFERENT probe tokens, and that is the whole reason this
// test has two parts. A word Singular rewrites gets a fresh allocation for the
// value whatever the code does, so it can only ever prove the key; the value is
// only pinnable when Singular hands its argument straight back. Checking the
// value with a rewritten word looks like coverage and is not: it leaves
// `Singular(t)` in place of `Singular(key)` — half the leak, restored — passing.
func TestStemMemoEntryDoesNotPinItsInput(t *testing.T) {
	resetStemMemo()

	// One long canonical string; both probes are windows onto it.
	long := Canonicalize("vulnerabilities-" + strings.Repeat("padding-", 200) + "tail")
	tokens := strings.FieldsFunc(long, func(r rune) bool { return r == '/' || unicode.IsSpace(r) })
	require.Equal(t, "vulnerabilities", tokens[0], "probe 1 must be a substring of the long string")
	require.Equal(t, "padding", tokens[1], "probe 2 must be a substring of the long string")

	// KEY. "vulnerabilities" is rewritten by Singular, so this half is about the
	// key alone.
	require.Equal(t, "vulnerability", Stem(tokens[0]))
	keyStored, ok := stemMemoKeyFor("vulnerabilities")
	require.True(t, ok)
	require.NotSame(t, unsafe.StringData(tokens[0]), unsafe.StringData(keyStored),
		"the memo key must be its own allocation, not a window onto the caller's buffer")

	// VALUE. "padding" is >3 chars, not -ics, and already singular, so Singular
	// returns its argument unchanged — which is the only case in which the value
	// can alias, and therefore the only case that can catch singularizing the
	// original instead of the clone.
	require.Equal(t, "padding", Stem(tokens[1]))
	v, loaded := stemMemo.Load("padding")
	require.True(t, loaded)
	require.NotSame(t, unsafe.StringData(tokens[1]), unsafe.StringData(v.(string)),
		"the memo value must be its own allocation too, even when Singular is a no-op")
}

// stemMemoKeyFor returns the string the memo actually STORED as the key for
// name — not name itself, which is the point: the two are equal and may or may
// not be the same allocation.
func stemMemoKeyFor(name string) (string, bool) {
	var found string
	var ok bool
	stemMemo.Range(func(k, _ any) bool {
		if k.(string) == name {
			found, ok = k.(string), true
			return false
		}
		return true
	})
	return found, ok
}

// Stem is called from a SQLite callback on pooled connections, so concurrent
// callers hit the memo at once. Run under -race.
func TestStemMemoIsSafeUnderConcurrency(t *testing.T) {
	resetStemMemo()

	toks := []string{"vulnerabilities", "analyses", "indices", "fallbacks", "clusters"}
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := toks[i%len(toks)]
			require.Equal(t, unmemoizedStem(tok), Stem(tok))
		}(i)
	}
	wg.Wait()

	require.Len(t, stemMemoEntries(), len(toks))
}
