// Package textnorm is the single definition of "the same token" for knomit.
//
// It exists because two subsystems must agree on that definition and cannot
// import each other. Domain matching lives in internal/store; the motif
// subject-word strip lives in internal/fact, which imports no internal
// package at all, by design. Two copies of a stemmer are two things that
// drift, and when they drift a motif that merely renames its own fact's
// subject slips past the strip — because "vulnerabilities" and
// "vulnerability" stopped being the same word on one side of the repo.
// One definition, two importers.
//
// Everything here is pure, deterministic and idempotent, and every rule in it
// was verified against the real knomit domain corpus. The canonical/token
// forms are derived state: authored tags stay in git, and nothing here is
// ever written back into a fact.
package textnorm

import (
	"strings"
	"sync"
	"unicode"

	"github.com/gertd/go-pluralize"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// caser performs Unicode case folding (language-neutral). Allocated once.
var caser = cases.Fold()

// pluralizer singularizes plural tokens to a match key. go-pluralize is a
// port of the widely-used JS `pluralize`, with a real irregular/exception table
// (not naive suffix rules), so it is symmetric and idempotent on the irregulars
// a hand-rolled or Porter/Snowball stemmer breaks: analyses≡analysis,
// indices≡index, matrices≡matrix, theses≡thesis. Allocated once; configured for
// the technical vocabulary in Stem's guards. Pure-Go, zero deps.
var pluralizer = pluralize.NewClient()

// Fold applies Unicode case folding and nothing else. It is the same folding
// Canonicalize uses, exposed on its own for callers that need case-insensitive
// equality without tokenization — mirroring a COLLATE NOCASE column, where
// de-hyphenizing would change what the column holds.
func Fold(s string) string { return caser.String(s) }

// Canonicalize normalises a string for matching and junction storage:
// NFC → case-fold → replace hyphens and Unicode whitespace with a single space →
// trim. Underscores are PRESERVED so identifier-like tags (commit_log) stay one
// token. Pure and idempotent.
func Canonicalize(s string) string {
	s = norm.NFC.String(s)
	s = caser.String(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // leading-trim: suppress leading spaces
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimRight(b.String(), " ")
}

// Stem normalises a token to its singular form as a MATCH-ONLY key — the
// result is never displayed or stored as a canonical tag, only used so two
// tokens collapse to the same key ("vulnerabilities" ≡ "vulnerability"). It
// delegates to go-pluralize, which is symmetric and idempotent on the
// irregulars a hand-rolled or Porter/Snowball stemmer breaks (analyses≡analysis,
// indices≡index, matrices≡matrix, theses≡thesis), verified against the real
// domain corpus.
//
// Two guards prevent over-singularizing non-plurals that merely end in 's'
// (any pluralizer treats a trailing 's' as plural, which is what we DON'T want
// for these — confirmed against real knomit domains):
//   - len <= 3: acronyms/identifiers (ai, aws, llm, tls) are never plurals.
//   - "...ics": -ics field/mass nouns (economics, robotics, metrics, ethics)
//     are singular; stripping to -ic would be wrong and asymmetric.
//
// Both guards are symmetric (applied identically at index and query time), so
// they never break matching; they only avoid mangling internal keys.
//
// MEMOIZED, and that is a performance fix rather than a design choice about
// meaning: go-pluralize's Singular runs a table of backtracking regexes and
// costs ~25µs per token, which is three to four orders of magnitude more than
// everything else on this path. That price used to be paid per token per motif
// per ROW, inside the knomit_motif_key SQL callback, on every request — the
// motif vocabulary of a single repo took ~40ms, and a five-mount lens paid it
// five times. Memoizing takes Stem to tens of nanoseconds — three orders of
// magnitude, and 0 allocations on the hit — and the lens vocabulary from
// ~133ms to ~9ms. See BenchmarkStem, which records the orders rather than the
// digits, because the warm figure moves run to run.
//
// It needs NO invalidation, ever. Stem is a pure function of its argument, so
// the memo is not derived corpus state that can go stale — it is the same
// answer, remembered. That is why it lives here on the primitive rather than
// as a per-branch cache in a consumer: one seam, and every caller — groupingKey,
// domain matching, the motif-term tiers in store.expandMotifQuery, the motif
// subject-word strip — gets it. Free-text search is NOT one of them: opts.Text
// goes to the embedder, and never through this package.
//
// Unbounded, deliberately, and matching store.branchCache's posture. What
// bounds it is worth stating exactly, because it is NOT only the corpus: the
// keys are single tokens from a corpus's own vocabulary PLUS whatever
// domain and motif filter tokens callers have asked about — ?domain= and
// ?motifs= reach here through store.canonicalizeDomain and the motif tiers, so
// a request can mint an entry. There is still no size to pick that would not
// be a guess about how big a corpus is, but see the Clone below for what that
// admission costs.
var stemMemo sync.Map // token → singular form

func Stem(t string) string {
	if len(t) <= 3 || strings.HasSuffix(t, "ics") {
		return t
	}
	if s, ok := stemMemo.Load(t); ok {
		return s.(string)
	}
	// CLONED BEFORE IT IS STORED, and that is about retention, not correctness.
	// Tokens hands us a strings.FieldsFunc substring, which shares the
	// canonicalized input's backing array — so storing it as-is would pin the
	// WHOLE canonical string for the entry's life, and Singular returning its
	// argument unchanged (the common case) would pin it through the value too.
	// One token off a long ?domain= would hold that whole filter value forever.
	// Cloning first, and singularizing the clone, means an entry retains its
	// own bytes and nothing else. It costs one allocation on a MISS; the hit
	// path above never reaches here.
	//
	// Racing callers may both compute this; Singular is pure, so they compute
	// the same string and the second Store is a no-op in everything but timing.
	key := strings.Clone(t)
	s := pluralizer.Singular(key)
	stemMemo.Store(key, s)
	return s
}

// Tokens returns the stemmed, de-duplicated token set of a canonical string,
// split on whitespace AND slash. Splitting on '/' too means a hierarchical
// tag's individual segments are each searchable as a word
// ("multi-tenant/auth" → ["multi","tenant","auth"]) — without it, a segment
// glued to a slash ("tenant/auth") is unreachable by its own middle word and
// the slash-hierarchy branch only matches whole prefixes. Order follows first
// appearance; callers treat it as a set. Pass the output of Canonicalize.
func Tokens(canonical string) []string {
	fields := strings.FieldsFunc(canonical, func(r rune) bool {
		return r == '/' || unicode.IsSpace(r)
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		st := Stem(f)
		if _, ok := seen[st]; ok {
			continue
		}
		seen[st] = struct{}{}
		out = append(out, st)
	}
	return out
}
