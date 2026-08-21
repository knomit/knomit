package fact

import (
	"fmt"
	"regexp"
	"strings"

	"knomit/internal/textnorm"
)

// MaxMotifs is the per-fact cap from the field contract (blueprint §1).
//
// CONSTANT CLASSIFICATION (roadmap MN13): this is a WRITE-DISCIPLINE BUDGET,
// not a corpus-property constant. It asserts nothing about any corpus's
// distribution — no cosine, no density, no frequency. It exists because the
// value of a motif is that it TRANSFERS, and an author allowed unlimited
// motifs stops choosing; three forces the choice. Changing it changes what we
// ask of authors, never what we claim about a repo.
const MaxMotifs = 3

// A motif is a 2–4 word kebab-case noun phrase. Both bounds are contract, not
// calibration: below two words a phrase is a bare noun that cannot name a
// mechanism, and above four it has become a sentence — which is the title's
// job, not the motif's. The upper bound is measured ground (designer ruling
// 2026-08-21); do not widen it to accommodate a phrasing.
const (
	minMotifWords = 2
	maxMotifWords = 4
)

// motifWord is one kebab segment: lowercase alphanumeric, at least one
// character. Digits are allowed so protocol- and version-shaped regularities
// ("http2-head-of-line") can be named.
var motifWord = regexp.MustCompile(`^[a-z0-9]+$`)

// ValidateMotifs enforces the two REJECTING halves of the motif contract:
// at most MaxMotifs entries, each a well-formed 2–4 word kebab-case phrase,
// no duplicates. It is called from SerializeFact and nowhere else — that is
// the a63ff254 single-entry-point pattern, and it is what makes every write
// path (learn, update, dedup-merge, prune, distill, discover) covered with no
// per-path code.
//
// The SILENT half of the contract — the subject-word strip — is deliberately
// NOT here: see StripSubjectMotifs. A caller writing a motif that merely
// renames its fact's subject has made an ordinary mistake and gets a quieter
// correction; a caller writing "onlyoneword" has misunderstood the field and
// should be told, and is an agent mid-call that can retry.
func ValidateMotifs(motifs []string) error {
	if len(motifs) > MaxMotifs {
		return fmt.Errorf("motifs: %d entries exceeds the maximum of %d", len(motifs), MaxMotifs)
	}
	seen := make(map[string]struct{}, len(motifs))
	for _, m := range motifs {
		if err := validateMotifShape(m); err != nil {
			return err
		}
		if _, dup := seen[m]; dup {
			return fmt.Errorf("motifs: duplicate entry %q", m)
		}
		seen[m] = struct{}{}
	}
	return nil
}

// validMotifShape reports whether m is a well-formed motif string.
func validMotifShape(m string) bool { return validateMotifShape(m) == nil }

func validateMotifShape(m string) error {
	if m == "" {
		return fmt.Errorf("motifs: empty entry")
	}
	if m != strings.TrimSpace(m) {
		return fmt.Errorf("motif %q: leading or trailing whitespace", m)
	}
	words := strings.Split(m, "-")
	if len(words) < minMotifWords || len(words) > maxMotifWords {
		return fmt.Errorf("motif %q: %d kebab-case words, want %d–%d",
			m, len(words), minMotifWords, maxMotifWords)
	}
	for _, w := range words {
		if !motifWord.MatchString(w) {
			return fmt.Errorf("motif %q: segment %q is not lowercase alphanumeric — motifs are kebab-case", m, w)
		}
	}
	return nil
}

// DropInvalidMotifs is the READ-side counterpart to ValidateMotifs: it drops
// what ValidateMotifs would reject instead of failing, preserving order.
//
// The asymmetry is the same one ParseFact already applies to refs and to
// origin, for the same reason stated there: this is a historical graph, and a
// version that was legal when it was committed must stay readable forever. A
// malformed motif costs a reader nothing — nothing consumes motifs yet, and
// later phases treat an unknown motif as a candidate that finds no partner.
// Losing the field beats losing the fact. SerializeFact still rejects, which
// is what stops the corpus accumulating more of them.
//
// Returns nil (not an empty slice) when nothing survives, so an all-invalid
// list is indistinguishable from an absent one — see Fact.Motifs on why nil
// and empty must stay the same thing.
func DropInvalidMotifs(motifs []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(motifs))
	for _, m := range motifs {
		if !validMotifShape(m) {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
		if len(out) == MaxMotifs {
			break
		}
	}
	return out
}

// StripSubjectMotifs returns f's motifs with every SUBJECT motif removed: one
// whose stemmed token set is a subset of the fact's own subject tokens
// (entities ∪ domain ∪ path). It never errors and never reports — a motif
// that merely renames its fact's subject is structurally impossible to store,
// and blueprint §2's Block B tells authors so up front, which is where that
// correction belongs.
//
// The SUBSET test, not equality, is the point.
// "antigravity-plugin-resolution" on a fact at
// kb/.../antigravity/plugin-dir-resolution/ contributes nothing a reader
// could not get from the path; "antigravity-shadowing" adds "shadowing", a
// shape another fact could carry, and survives. The question the test asks
// is: does this phrase say anything this fact has not already said about
// ITSELF?
//
// Returns nil when nothing survives, so an all-stripped list is
// indistinguishable from an absent one (see Fact.Motifs).
func StripSubjectMotifs(f Fact) []string {
	subject := subjectTokens(f)
	var out []string
	for _, m := range f.Motifs {
		if isSubjectMotif(m, subject) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// subjectTokens is the stemmed token set of everything the fact already says
// about its own subject: authored entities, domain tags, and path segments.
//
// The path is included because a fact's location IS a subject claim — the
// ontology category is chosen to describe what the fact is about — and
// because the most common subject motif in the wild is the category slug
// re-typed. The ontology root and the uuid segment ride along harmlessly:
// they are stemmed tokens like any other, and no motif of 2+ words is a
// subset of {kb} or of a hex id.
func subjectTokens(f Fact) map[string]struct{} {
	set := make(map[string]struct{})
	add := func(s string) {
		for _, tok := range textnorm.Tokens(textnorm.Canonicalize(s)) {
			set[tok] = struct{}{}
		}
	}
	for _, e := range f.Entities {
		add(e)
	}
	for _, d := range f.Domain {
		add(d)
	}
	// Trim the extension first so "e5d04257.md" does not contribute a
	// spurious "md" token — that describes the file format, not the fact.
	// Canonicalize de-hyphenizes and Tokens splits on '/', so path segments
	// and their words arrive already separated.
	add(strings.TrimSuffix(f.path, ".md"))
	return set
}

// isSubjectMotif reports whether every stemmed token of motif is already in
// the fact's subject set.
func isSubjectMotif(motif string, subject map[string]struct{}) bool {
	toks := textnorm.Tokens(textnorm.Canonicalize(motif))
	if len(toks) == 0 {
		return false
	}
	for _, t := range toks {
		if _, ok := subject[t]; !ok {
			return false
		}
	}
	return true
}
