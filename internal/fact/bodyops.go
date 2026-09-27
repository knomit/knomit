package fact

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Body op names accepted by ApplyBodyOps.
const (
	OpStrReplace = "str_replace"
	OpAppend     = "append"
)

// maxReportedOffsets caps the offset list in an ambiguous-anchor error. It
// bounds the message, not the matching: the count is always exact.
const maxReportedOffsets = 10

// maxPrefixEcho caps how much of the matched old_str prefix a zero-match error
// quotes back. The quote is the tail of the caller's own old_str, for
// orientation; the offset is what locates it.
const maxPrefixEcho = 40

// BodyOp is one edit to a fact body. NewStr is a pointer so an omitted new_str
// is an error rather than a silent deletion; "" is the explicit delete.
type BodyOp struct {
	Op     string  `json:"op"`
	OldStr string  `json:"old_str,omitempty"`
	NewStr *string `json:"new_str,omitempty"`
	Text   string  `json:"text,omitempty"`
}

// BodyOpDelta is the byte-length change one op made to the body.
type BodyOpDelta struct {
	Op    string `json:"op"`
	Delta int    `json:"delta"`
}

// ApplyBodyOps applies ops in order to body, each against the result of the
// ones before it, and returns the final body and each op's byte delta. It is
// all-or-nothing: on any error the caller gets no body and must write nothing.
// Every error names the failing op by its zero-based index.
//
// Matching is byte-exact. A str_replace anchor must occur exactly once,
// counting overlapping occurrences — "aa" in "aaa" is two matches. There is
// deliberately no regex, no replace-all, no occurrence index and no whitespace
// or Unicode normalisation: an anchor that does not match exactly is refused
// with diagnostics, never repaired. The diagnostics may be heuristic; the
// application never is.
func ApplyBodyOps(body string, ops []BodyOp) (string, []BodyOpDelta, error) {
	if len(ops) == 0 {
		return "", nil, errors.New("ops is empty: send at least one op, or omit ops")
	}
	deltas := make([]BodyOpDelta, 0, len(ops))
	for i, op := range ops {
		next, err := applyBodyOp(body, op)
		if err != nil {
			return "", nil, fmt.Errorf("op %d: %w", i, err)
		}
		deltas = append(deltas, BodyOpDelta{Op: op.Op, Delta: len(next) - len(body)})
		body = next
	}
	return body, deltas, nil
}

func applyBodyOp(body string, op BodyOp) (string, error) {
	switch op.Op {
	case OpStrReplace:
		if op.Text != "" {
			return "", errors.New("str_replace does not take text")
		}
		if op.OldStr == "" {
			return "", errors.New("str_replace needs a non-empty old_str")
		}
		if op.NewStr == nil {
			return "", errors.New(`str_replace needs new_str (send "" to delete old_str)`)
		}
		offsets := matchOffsets(body, op.OldStr)
		switch len(offsets) {
		case 1:
			at := offsets[0]
			return body[:at] + *op.NewStr + body[at+len(op.OldStr):], nil
		case 0:
			return "", errors.New(zeroMatchDiagnostic(body, op.OldStr))
		default:
			return "", errors.New(multiMatchDiagnostic(offsets))
		}
	case OpAppend:
		if op.OldStr != "" || op.NewStr != nil {
			return "", errors.New("append takes only text, not old_str or new_str")
		}
		if op.Text == "" {
			return "", errors.New("append needs a non-empty text")
		}
		return appendParagraph(body, op.Text), nil
	default:
		return "", fmt.Errorf("unknown op %q (want %q or %q)", op.Op, OpStrReplace, OpAppend)
	}
}

// appendParagraph adds text to body as a new paragraph: it inserts only the
// newlines needed for one blank line between them, counting the newlines body
// already ends with and the ones text already starts with. Nothing the caller
// sent is removed.
func appendParagraph(body, text string) string {
	if body == "" {
		return text
	}
	have := len(body) - len(strings.TrimRight(body, "\n"))
	have += len(text) - len(strings.TrimLeft(text, "\n"))
	if have >= 2 {
		return body + text
	}
	return body + strings.Repeat("\n", 2-have) + text
}

// matchOffsets returns every byte offset at which needle starts in haystack,
// overlapping occurrences included.
func matchOffsets(haystack, needle string) []int {
	var out []int
	for from := 0; from <= len(haystack)-len(needle); {
		i := strings.Index(haystack[from:], needle)
		if i < 0 {
			break
		}
		out = append(out, from+i)
		from += i + 1
	}
	return out
}

func multiMatchDiagnostic(offsets []int) string {
	shown := offsets
	more := ""
	if len(shown) > maxReportedOffsets {
		shown = shown[:maxReportedOffsets]
		more = fmt.Sprintf(" and %d more", len(offsets)-maxReportedOffsets)
	}
	return fmt.Sprintf("%d matches at offsets %v%s; old_str must match exactly once — widen it with surrounding text",
		len(offsets), shown, more)
}

// zeroMatchDiagnostic explains why old_str is absent: the longest prefix of it
// that does occur in body, where, and the first character after that prefix on
// each side. The usual culprits — smart quotes, an em dash for "--", a
// non-breaking space, trailing whitespace — show up as differing code points.
func zeroMatchDiagnostic(body, oldStr string) string {
	// Prefix lengths on rune boundaries, so the divergent character is whole.
	cuts := []int{0}
	for i := range oldStr {
		if i > 0 {
			cuts = append(cuts, i)
		}
	}
	// "prefix of length cuts[k] occurs" is monotone in k: find the largest k.
	k := sort.Search(len(cuts), func(k int) bool {
		return !strings.Contains(body, oldStr[:cuts[k]])
	}) - 1
	n := cuts[k]
	want, _ := utf8.DecodeRuneInString(oldStr[n:])

	if n == 0 {
		return fmt.Sprintf("0 matches; not even the first character of old_str, %s, occurs in the body",
			describeRune(want))
	}
	at := strings.Index(body, oldStr[:n])
	echo := oldStr[:n]
	if utf8.RuneCountInString(echo) > maxPrefixEcho {
		r := []rune(echo)
		echo = "…" + string(r[len(r)-maxPrefixEcho:])
	}
	got := "end of body"
	if at+n < len(body) {
		r, _ := utf8.DecodeRuneInString(body[at+n:])
		got = describeRune(r)
	}
	return fmt.Sprintf("0 matches; the longest prefix of old_str found is %d bytes (%q) at offset %d; "+
		"next character: old_str has %s, body has %s",
		n, echo, at, describeRune(want), got)
}

func describeRune(r rune) string {
	return fmt.Sprintf("U+%04X %s", r, strconv.QuoteRune(r))
}
