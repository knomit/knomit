// Package synthesize — structural duplicate detection (#127).
//
// The title-KNN shortlist finds pairs that are CLOSE IN THE VECTOR SPACE. The
// duplicates that survive longest are the ones it cannot reach: two records of
// one event, filed under two freeform categories, whose titles are paraphrases
// rather than neighbours. Measured on the live core corpus, six confirmed
// duplicate pairs each had a partner ~24 unrelated facts closer to it by title
// cosine than nothing — and the whole point is that similarity is not the only
// evidence a corpus carries about identity.
//
// This file adds the other evidence, in two shapes:
//
//   - PATH IDENTITY. Two paths that normalise to one identity — the same
//     tokens in a different order (devops/gitlab vs gitlab/devops), or one
//     path's tokens a subset of the other's (vulnerabilities/cisco vs
//     vulnerabilities/networking/cisco). Casefolding, hyphen splitting and
//     singular/plural collapse come from textnorm; a UUID filename is dropped,
//     because a generated name is not something two facts can agree about,
//     while a slug filename is content and stays.
//
//   - RARE IDENTIFIER TOKENS. Two facts that share a token which is
//     identifier-SHAPED (it carries a digit) and rare in THIS corpus. A CVE
//     id, a ticket number, a version. The shape test is lexical and says
//     nothing about any corpus; the rarity test is read entirely off this
//     repo's own distribution, per the corpus-property-constants principle —
//     there is no rarity number here to configure, and a literal `CVE-\d+`
//     pattern would be exactly the forbidden thing, a corpus property wearing
//     a regex.
//
// Detection only. The pairs join the standing shortlist and are judged and
// merged by the machinery that already exists.
package synthesize

import (
	"path"
	"sort"
	"strings"

	"knomit/internal/store"
	"knomit/internal/textnorm"
)

// maxStructuralPairs is a RESOURCE BUDGET on one refresh's writes: the most
// structurally matched pairs a single session will mint.
//
// Not a claim about how many duplicates a corpus contains (MN13) — it bounds
// what one pass may cost when a corpus's token distribution turns out to be
// degenerate, in the same class as shortlistOverfetch. Pairs beyond it are not
// lost, only deferred: the next session's refresh re-derives from the same
// corpus and mints the ones that are still missing.
const maxStructuralPairs = 500

// identityIndex is the corpus's own structural evidence, derived once per
// refresh.
type identityIndex struct {
	// pathTokens is every fact's full normalised path token set, for the
	// subset test.
	pathTokens map[int64]map[string]struct{}
	// discriminating maps a fact to the tokens that are more specific than
	// this corpus's typical path token — the ones a match may rest on.
	discriminating map[int64][]string
	// identifiers maps a fact to its rare identifier-shaped tokens.
	identifiers map[int64][]string
	// byToken is the inverted index over both discriminating sets, so a
	// candidate scan never has to compare every fact with every other.
	byToken map[string][]int64
}

// buildIdentityIndex derives the structural evidence for a whole corpus.
//
// Both rarity cuts are the corpus's own FACT-WEIGHTED MEDIAN document
// frequency: the df of the token a randomly chosen occurrence carries. That is
// the right centre for this question and the distinct-token median is not — a
// corpus's token vocabulary is dominated by its hapax tail, so a median over
// DISTINCT tokens lands at one or two and would reject `cisco` (df 12) as
// insufficiently rare while accepting nothing useful. Weighting by occurrence
// asks "is this token more specific than what a path segment usually is?",
// which is the question, and it is answered entirely by the corpus.
func buildIdentityIndex(titles map[int64]store.LiveFactTitle) *identityIndex {
	ix := &identityIndex{
		pathTokens:     make(map[int64]map[string]struct{}, len(titles)),
		discriminating: make(map[int64][]string, len(titles)),
		identifiers:    make(map[int64][]string, len(titles)),
		byToken:        map[string][]int64{},
	}
	if len(titles) == 0 {
		return ix
	}

	pathDF := map[string]int{}
	idDF := map[string]int{}
	rawPath := make(map[int64][]string, len(titles))
	rawID := make(map[int64][]string, len(titles))

	for id, t := range titles {
		pt := pathTokensOf(t.Path)
		rawPath[id] = pt
		set := make(map[string]struct{}, len(pt))
		for _, tok := range pt {
			set[tok] = struct{}{}
			pathDF[tok]++
		}
		ix.pathTokens[id] = set

		idt := identifierTokens(t.Path, t.Title)
		rawID[id] = idt
		for _, tok := range idt {
			idDF[tok]++
		}
	}

	pathCut := factWeightedMedianDF(rawPath, pathDF)
	idCut := factWeightedMedianDF(rawID, idDF)

	for id := range titles {
		for _, tok := range rawPath[id] {
			// Strictly below the typical token, and carried by someone else:
			// a hapax can match nothing, so indexing it is pure cost.
			if pathDF[tok] < pathCut && pathDF[tok] >= 2 {
				ix.discriminating[id] = append(ix.discriminating[id], tok)
				ix.byToken[tok] = append(ix.byToken[tok], id)
			}
		}
		for _, tok := range rawID[id] {
			if idDF[tok] < idCut && idDF[tok] >= 2 {
				ix.identifiers[id] = append(ix.identifiers[id], tok)
				ix.byToken[tok] = append(ix.byToken[tok], id)
			}
		}
	}
	for tok := range ix.byToken {
		slice := ix.byToken[tok]
		sort.Slice(slice, func(i, j int) bool { return slice[i] < slice[j] })
		ix.byToken[tok] = slice
	}
	return ix
}

// factWeightedMedianDF is the df of the token a randomly chosen OCCURRENCE
// carries, over the tokens that could match anything at all — the corpus's own
// answer to "how specific is a shared token, typically".
//
// Two choices here, and both were wrong in an earlier version:
//
// WEIGHTED BY OCCURRENCE, not over distinct tokens. A corpus's vocabulary is
// dominated by its hapax tail, so a median over DISTINCT tokens lands at one or
// two and would reject `cisco` (df 12 on the live corpus) as insufficiently
// rare while accepting nothing useful.
//
// OVER df >= 2 ONLY. A token carried by exactly one fact can never pair with
// anything, so counting its occurrences measures a population the question is
// not about — and there are a great many of them (every generated filename,
// every one-off number). Including them dragged the median to 1, at which point
// `< median` is unsatisfiable and the whole rare-token route silently found
// nothing. That is the shape of failure this issue is about, so it gets a test:
// TestStructural_RareIdentifierToken fails outright if this filter is dropped.
//
// Returns 0 for a corpus with no shared tokens, which makes every rarity test
// false: with no distribution to read, nothing is known to be rare, and
// inventing a fallback number is exactly the forbidden constant.
//
// KNOWN BLIND SPOT (review LOW-1, accepted): on a corpus where more than half
// of all matchable token-occurrences sit on df==2 tokens, this returns 2, and
// the caller's `df < cut && df >= 2` band is then empty — both structural
// routes find nothing. That is graceful (no worse than having no structural
// detection at all) and needs an atypical distribution: real corpora repeat
// their freeform path prefixes, and those are high-df tokens that pull the
// median up. Recorded rather than fixed, because every fix for it is a floor
// or a fallback — i.e. a corpus-property constant (MN13) — and the failure is
// silence rather than a wrong merge.
func factWeightedMedianDF(perFact map[int64][]string, df map[string]int) int {
	var occurrences []int
	for _, toks := range perFact {
		for _, t := range toks {
			if df[t] < 2 {
				continue
			}
			occurrences = append(occurrences, df[t])
		}
	}
	if len(occurrences) == 0 {
		return 0
	}
	sort.Ints(occurrences)
	return occurrences[len(occurrences)/2]
}

// pathTokensOf normalises one fact path into match tokens.
//
// The extension goes, and a UUID FILENAME goes with it: the uuid is minted per
// fact (conventions/synthesize/normalize-fact-path-uuid), so it is by
// construction something no two facts can share, and keeping it would only add
// a token that is guaranteed not to match. A SLUG filename stays — an author
// who named the file said something about the fact, and that is content.
func pathTokensOf(p string) []string {
	p = strings.TrimSuffix(p, path.Ext(p))
	if isGeneratedName(path.Base(p)) {
		p = path.Dir(p)
	}
	return textnorm.Tokens(textnorm.Canonicalize(p))
}

// isGeneratedName reports whether a filename is a minted uuid rather than an
// authored slug: all hex, and long enough not to be a word.
func isGeneratedName(base string) bool {
	if len(base) < 8 {
		return false
	}
	for _, r := range base {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// identifierTokens returns the identifier-SHAPED tokens of a fact's path and
// title: tokens carrying at least one digit.
//
// The shape test is the generalisation of "extract the CVE key" that carries no
// claim about this corpus. A CVE id, a ticket number, a build number and a
// version all pass it; ordinary prose does not. Which of them is rare enough to
// match on is decided afterwards, by the corpus.
func identifierTokens(p, title string) []string {
	toks := textnorm.Tokens(textnorm.Canonicalize(strings.TrimSuffix(p, path.Ext(p)) + " " + title))
	out := make([]string, 0, 4)
	for _, t := range toks {
		if hasDigit(t) {
			out = append(out, t)
		}
	}
	return out
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// structuralMatch is one detected pair, before it is scored and stored.
type structuralMatch struct {
	a, b int64
	kind string
}

// structuralCandidates finds the pairs the title axis cannot reach.
//
// Only pairs TOUCHING the rescanned set are returned, mirroring the title KNN's
// own delta discipline: a pair between two facts that neither changed nor were
// requeued was already minted by the session that first saw them.
func (ix *identityIndex) structuralCandidates(requeue []int64) []structuralMatch {
	inRequeue := make(map[int64]struct{}, len(requeue))
	for _, id := range requeue {
		inRequeue[id] = struct{}{}
	}

	seen := map[[2]int64]string{}
	var out []structuralMatch
	// Deterministic: requeue is already sorted by the caller, and each token's
	// posting list is sorted, so the same corpus yields the same pairs in the
	// same order however Go iterates its maps.
	for _, id := range requeue {
		for _, tok := range append(append([]string{}, ix.discriminating[id]...), ix.identifiers[id]...) {
			for _, other := range ix.byToken[tok] {
				if other == id {
					continue
				}
				// Both sides in the requeue would emit the pair twice; the
				// lower id owns it.
				if _, alsoRescanned := inRequeue[other]; alsoRescanned && other < id {
					continue
				}
				key := [2]int64{min64(id, other), max64(id, other)}
				if _, dup := seen[key]; dup {
					continue
				}
				kind, ok := ix.classify(id, other)
				if !ok {
					continue
				}
				seen[key] = kind
				out = append(out, structuralMatch{a: key[0], b: key[1], kind: kind})
				if len(out) >= maxStructuralPairs {
					return out
				}
			}
		}
	}
	return out
}

// classify decides WHY two facts match, preferring the stronger evidence.
//
// Path identity first: two paths that normalise to one identity are making a
// claim about the same subject with the corpus's own filing as the witness. A
// shared rare identifier is the weaker, wider net.
func (ix *identityIndex) classify(a, b int64) (string, bool) {
	if ix.pathIdentity(a, b) {
		return store.MatchPathIdentity, true
	}
	if sharesToken(ix.identifiers[a], ix.identifiers[b]) {
		return store.MatchRareToken, true
	}
	return "", false
}

// pathIdentity holds when one path's token set contains the other's — equality
// included — and the containment rests on something specific.
//
// Equality is the segment-INVERSION case (devops/gitlab vs gitlab/devops
// normalise to one set). Proper containment is the prefix-EXTENSION case
// (vulnerabilities/cisco inside vulnerabilities/networking/cisco). Both are
// required to share at least one discriminating token, or every fact filed
// under the corpus's common prefix would match every other.
func (ix *identityIndex) pathIdentity(a, b int64) bool {
	sa, sb := ix.pathTokens[a], ix.pathTokens[b]
	if len(sa) == 0 || len(sb) == 0 {
		return false
	}
	if !tokenSubsetOf(sa, sb) && !tokenSubsetOf(sb, sa) {
		return false
	}
	return sharesToken(ix.discriminating[a], ix.discriminating[b])
}

func tokenSubsetOf(small, large map[string]struct{}) bool {
	if len(small) > len(large) {
		return false
	}
	for t := range small {
		if _, ok := large[t]; !ok {
			return false
		}
	}
	return true
}

func sharesToken(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, t := range a {
		set[t] = struct{}{}
	}
	for _, t := range b {
		if _, ok := set[t]; ok {
			return true
		}
	}
	return false
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
