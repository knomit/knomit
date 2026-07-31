// internal/okf/slug.go
package okf

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

const slugMaxLen = 60

// Slug builds the bundle filename for a fact: a slugified, truncated title
// plus "-<uuid8>.md". uuid8 is ALWAYS present. See the design's 4-step rule.
func Slug(title, categorySegment, uuid8 string) string {
	s := slugify(title)
	if s == "" {
		s = slugify(categorySegment)
	}
	if s == "" {
		return uuid8 + ".md"
	}
	return s + "-" + uuid8 + ".md"
}

// slugify lowercases, folds to ASCII, collapses non-[a-z0-9] runs to single
// hyphens, trims, and truncates to slugMaxLen at a hyphen boundary.
func slugify(in string) string {
	folded := foldASCII(in)
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(folded) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
		} else if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	s := strings.Trim(b.String(), "-")
	return truncateAtHyphen(s)
}

// truncateAtHyphen truncates s to slugMaxLen. If a hyphen exists at or before
// slugMaxLen, cut at the last such hyphen (never mid-word). Otherwise hard-cut
// at slugMaxLen (a first word longer than the limit).
func truncateAtHyphen(s string) string {
	if len(s) <= slugMaxLen {
		return s
	}
	head := s[:slugMaxLen]
	if i := strings.LastIndexByte(head, '-'); i > 0 {
		return head[:i]
	}
	return head
}

// foldASCII strips diacritics by decomposing to NFD and dropping combining marks.
func foldASCII(in string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, in)
	if err != nil {
		return in
	}
	return out
}
