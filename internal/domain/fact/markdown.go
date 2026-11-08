package fact

import (
	"errors"
	"fmt"
	"strings"
)

// CheckFenceEdit refuses an edit that UNBALANCES the body's triple-backtick
// fences: before has an even number of fence lines and after has an odd one.
// An unclosed fence turns everything after it into code, so one stray line
// silently breaks the rest of the fact's rendering.
//
// It judges the edit, not the result. Valid CommonMark can hold an odd count —
// a ```` fence quoting a ``` line, ``` inside a ~~~ fence, indented code, an
// inline ```x``` at the start of a line — and a fact like that must stay
// editable, so a body that was already odd is never refused here.
//
// A fence line is any line whose first non-blank characters are "```". The
// rule is a count, not a CommonMark parse, so its answer is deterministic.
// knomit_update is the only write path that runs it.
func CheckFenceEdit(before, after string) error {
	was, _ := fenceLines(before)
	now, last := fenceLines(after)
	if was%2 == 0 && now%2 != 0 {
		return fmt.Errorf("the edit unbalances the body's code fences: %d ``` fence lines, the last at line %d — close the fence or remove the stray one",
			now, last)
	}
	return nil
}

// fenceLines counts the fence lines in body and returns the 1-based number of
// the last one.
func fenceLines(body string) (count, last int) {
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			count++
			last = i + 1
		}
	}
	return count, last
}

// WikiLink is one `[[...]]` occurrence found in prose by WikiLinks.
type WikiLink struct {
	Text string // the occurrence as written, brackets included
	Line int    // 1-based line number in the body
}

// WikiLinks returns every `[[...]]` in body that sits in prose: outside code.
// Code is exempt because `[[...]]` is legitimate there — a TOML
// array-of-tables header `[[oauth.client]]`, a POSIX class `[[:space:]]`.
//
// An occurrence runs from a `[[` to the first `]]` after it on the same line,
// with something other than spaces between them; single brackets inside are
// part of it (`[[the [x] fact]]`). A lone `[[` with no `]]` on its line is not
// one, and neither is `[[ ]]`.
//
// What counts as code follows CommonMark closely enough for this rule:
//
//   - A fenced block: a line whose content, after any blockquote `>` and list
//     markers (`-`, `*`, `+`, `1.`, `1)`), starts with three or more backticks
//     or tildes. A backtick line with another backtick after the run is an
//     inline span instead. The block closes on a line holding only a run of
//     the same character at least as long; an unclosed block runs to the end
//     of the body.
//   - An indented block: a line indented four or more columns (a tab counts as
//     four) that does not continue a paragraph and is not inside a list item.
//   - An inline code span: a backtick run closed by a run of the same length
//     in the same paragraph. A blank line, a heading line or a fence ends the
//     paragraph. A backtick escaped with `\` does not open a span; inside a
//     span a backslash is literal, so the backtick after it can close one, as
//     in CommonMark.
//     An unmatched run is literal text.
//
// This is not CheckFenceEdit's rule. fenceLines counts only ``` lines, at any
// indentation and of any length, ```x``` included, and it ignores ~~~ and list
// markers. The two answer different questions — whether an edit unbalances the
// fences, and what is code — and the difference is harmless here: a line this
// scanner reads as a fence and fenceLines does not can only hide text, so it
// lowers the count, never raises it.
//
// Known limits, left as they are: HTML code (`<code>`, `<pre>`) is prose here;
// fullwidth brackets (［［x］］) are not brackets; and, as in CommonMark, an
// unclosed fence turns the rest of a NEW body into code, so `[[...]]` after
// it is not counted.
func WikiLinks(body string) []WikiLink {
	mask := proseMask(body)
	var out []WikiLink
	for from := 0; ; {
		i := strings.Index(mask[from:], "[[")
		if i < 0 {
			break
		}
		i += from
		lineEnd := strings.IndexByte(mask[i:], '\n')
		if lineEnd < 0 {
			lineEnd = len(mask)
		} else {
			lineEnd += i
		}
		j := strings.Index(mask[i+2:lineEnd], "]]")
		if j < 0 {
			from = lineEnd
			continue
		}
		end := i + 2 + j + 2
		text := body[i:end]
		if strings.TrimSpace(text[2:len(text)-2]) == "" {
			from = i + 2
			continue
		}
		out = append(out, WikiLink{Text: text, Line: 1 + strings.Count(body[:i], "\n")})
		from = end
	}
	return out
}

// ErrWikiLinkInBody marks a refusal by CheckWikiLinks or CheckWikiLinksUpdate.
var ErrWikiLinkInBody = errors.New("wiki link in fact body")

// wikiLinkAdvice is the part of every refusal that says what to send instead.
const wikiLinkAdvice = "knomit has no link syntax in a body: it neither resolves nor renders [[...]], and every reference, to another fact or to an external source, goes in refs. " +
	"Put the cited fact's path (kb/<topic>/<category>/<id>.md, as knomit_query returned it) in refs, and keep the body plain text. " +
	"To show [[...]] literally, as in TOML or a regex, put it in an inline code span or a fenced code block."

// CheckWikiLinks refuses a body that holds any `[[...]]` in prose (see
// WikiLinks). Every write that creates a fact runs it on the body it writes.
func CheckWikiLinks(body string) error {
	return checkWikiLinks(body, "")
}

// CheckWikiLinksUpdate is CheckWikiLinks for a write that changes an existing
// fact. It judges the RESULTING body only, whatever the fact held before: an
// update of a fact that already carries `[[...]]` must remove them, so a
// legacy fact cannot be updated, on any field, until its body is fixed. Its
// error says so.
func CheckWikiLinksUpdate(body string) error {
	return checkWikiLinks(body, "An update must leave no [[...]] outside code, including ones the fact already had. ")
}

func checkWikiLinks(body, note string) error {
	links := WikiLinks(body)
	if len(links) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the body contains %s. %s%s", ErrWikiLinkInBody, listWikiLinks(links), note, wikiLinkAdvice)
}

// listWikiLinks renders up to three occurrences with their line numbers.
func listWikiLinks(links []WikiLink) string {
	const shown = 3
	parts := make([]string, 0, shown)
	for i, l := range links {
		if i == shown {
			parts = append(parts, fmt.Sprintf("and %d more", len(links)-shown))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (line %d)", l.Text, l.Line))
	}
	return strings.Join(parts, ", ")
}

// proseMask returns body with every byte of code — fenced and indented blocks,
// inline code spans — replaced by a space, newlines kept, so offsets into the
// mask are offsets into body. See WikiLinks for the rules.
func proseMask(body string) string {
	mask := []byte(body)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			if mask[i] != '\n' {
				mask[i] = ' '
			}
		}
	}
	var (
		fenceChar byte // 0 outside a fenced block
		fenceLen  int
		paraStart = -1   // start of the current prose paragraph, -1 if none
		inList    bool   // a list item is open: indented lines continue it
		prevBlank = true // the previous line was blank (or there was none)
	)
	endPara := func(at int) {
		if paraStart >= 0 {
			blankCodeSpans(body[paraStart:at], paraStart, blank)
			paraStart = -1
		}
	}
	for start := 0; start < len(body); {
		end := strings.IndexByte(body[start:], '\n')
		if end < 0 {
			end = len(body)
		} else {
			end += start
		}
		raw := body[start:end]
		content, listItem := stripContainers(raw)
		ch, n := fenceRun(content)
		blankLine := strings.TrimSpace(raw) == ""
		switch {
		case fenceChar != 0:
			blank(start, end)
			if ch == fenceChar && n >= fenceLen && strings.TrimSpace(content[n:]) == "" {
				fenceChar = 0
			}
		case n > 0:
			endPara(start)
			blank(start, end)
			fenceChar, fenceLen = ch, n
		case blankLine:
			endPara(start)
		case !inList && paraStart < 0 && indentColumns(raw) >= 4:
			// An indented code block: it cannot interrupt a paragraph, and in
			// a list an indented line continues the item instead.
			blank(start, end)
		case isATXHeading(content):
			endPara(start)
			blankCodeSpans(raw, start, blank)
		default:
			if paraStart < 0 {
				paraStart = start
			}
		}
		if !blankLine && fenceChar == 0 {
			switch {
			case listItem:
				inList = true
			case prevBlank && indentColumns(raw) == 0:
				inList = false
			}
		}
		prevBlank = blankLine
		start = end + 1
	}
	endPara(len(body))
	return string(mask)
}

// stripContainers removes a line's leading blockquote markers and list
// markers, with the spaces around them, and reports whether it held a list
// marker. "> - ```toml" becomes "```toml".
func stripContainers(line string) (string, bool) {
	listItem := false
	for {
		s := strings.TrimLeft(line, " \t")
		switch {
		case strings.HasPrefix(s, ">"):
			line = s[1:]
			continue
		case len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && (s[1] == ' ' || s[1] == '\t'):
			line, listItem = s[2:], true
			continue
		}
		d := 0
		for d < len(s) && d < 9 && s[d] >= '0' && s[d] <= '9' {
			d++
		}
		if d > 0 && d+1 < len(s) && (s[d] == '.' || s[d] == ')') && (s[d+1] == ' ' || s[d+1] == '\t') {
			line, listItem = s[d+2:], true
			continue
		}
		return s, listItem
	}
}

// indentColumns is the width of line's leading whitespace, a tab counting as
// four columns.
func indentColumns(line string) int {
	n := 0
	for _, c := range line {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// isATXHeading reports whether line (containers stripped) is a `#` heading.
func isATXHeading(line string) bool {
	h := 0
	for h < len(line) && line[h] == '#' {
		h++
	}
	return h >= 1 && h <= 6 && (h == len(line) || line[h] == ' ' || line[h] == '\t')
}

// fenceRun reports the fence character and run length line starts with, or
// (0, 0) when it does not start with three or more backticks or tildes.
func fenceRun(line string) (byte, int) {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return 0, 0
	}
	// CommonMark: a backtick fence's info string holds no backtick, so a
	// line like ```x``` is an inline code span, not a fence.
	if line[0] == '`' && strings.IndexByte(line[n:], '`') >= 0 {
		return 0, 0
	}
	return line[0], n
}

// blankCodeSpans blanks every inline code span in para, which starts at
// offset base in the body. A backtick escaped by an odd run of backslashes
// does not open a span; a closing run is matched literally, as in CommonMark.
func blankCodeSpans(para string, base int, blank func(from, to int)) {
	for i := 0; i < len(para); {
		if para[i] != '`' {
			i++
			continue
		}
		if escaped(para, i) {
			i++ // a literal backtick; the rest of its run may still open one
			continue
		}
		n := backtickRun(para, i)
		closeAt := -1
		for j := i + n; j < len(para); {
			if para[j] != '`' {
				j++
				continue
			}
			m := backtickRun(para, j)
			if m == n {
				closeAt = j
				break
			}
			j += m
		}
		if closeAt < 0 {
			i += n // unmatched: literal backticks
			continue
		}
		blank(base+i, base+closeAt+n)
		i = closeAt + n
	}
}

// escaped reports whether s[i] is preceded by an odd number of backslashes.
func escaped(s string, i int) bool {
	k := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		k++
	}
	return k%2 == 1
}

func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}
