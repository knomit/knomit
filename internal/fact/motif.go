package fact

import (
	"fmt"
	"regexp"
	"strings"
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
