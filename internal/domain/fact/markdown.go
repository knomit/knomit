package fact

import (
	"errors"
	"fmt"
	"regexp"
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

// wikiLinkRe matches `[[x]]` on one line, with no bracket inside. It is only
// ever run over prose: code has been blanked out by proseMask first.
var wikiLinkRe = regexp.MustCompile(`\[\[[^\[\]\n]+\]\]`)

// WikiLinks returns every `[[...]]` in body that sits in prose: outside fenced
// code blocks (``` or ~~~) and outside inline code spans. Code is exempt
// because `[[...]]` is legitimate there — a TOML array-of-tables header
// `[[oauth.client]]`, a POSIX class `[[:space:]]`.
//
// An occurrence needs both halves on one line and something other than spaces
// between them: a lone `[[` with no `]]` is not one, and neither is `[[ ]]`.
//
// The fence rule matches fenceLines: a fence line is one whose first
// non-blank characters are three or more backticks or tildes, at any
// indentation (a backtick line with another backtick after the run is an
// inline span instead, as in CommonMark). A block closes on a line holding only a run of the same
// character at least as long; an unclosed block runs to the end of the body,
// as in CommonMark. A code span is a backtick run closed by a run of the same
// length within the same paragraph; an unmatched run is literal text.
func WikiLinks(body string) []WikiLink {
	mask := proseMask(body)
	var out []WikiLink
	for _, loc := range wikiLinkRe.FindAllStringIndex(mask, -1) {
		text := body[loc[0]:loc[1]]
		if strings.TrimSpace(text[2:len(text)-2]) == "" {
			continue
		}
		out = append(out, WikiLink{Text: text, Line: 1 + strings.Count(body[:loc[0]], "\n")})
	}
	return out
}

// ErrWikiLinkInBody marks a refusal by CheckWikiLinks or CheckWikiLinkEdit.
var ErrWikiLinkInBody = errors.New("wiki link in fact body")

// wikiLinkAdvice is the part of every refusal that says what to send instead.
const wikiLinkAdvice = "knomit has no inline link syntax: it neither resolves nor renders [[...]], and a fact links to another only through refs. " +
	"Put the cited fact's path (kb/<topic>/<category>/<id>.md, as knomit_query returned it) in refs, and keep the body plain text. " +
	"To show [[...]] literally, as in TOML or a regex, put it in an inline code span or a fenced code block."

// CheckWikiLinks refuses a NEW body that carries any `[[...]]` in prose (see
// WikiLinks). Every write that creates a fact runs it.
func CheckWikiLinks(body string) error {
	links := WikiLinks(body)
	if len(links) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the body contains %s. %s", ErrWikiLinkInBody, listWikiLinks(links), wikiLinkAdvice)
}

// CheckWikiLinkEdit refuses an edit whose result carries MORE `[[...]]` in
// prose than the body it replaces. Like CheckFenceEdit it judges the edit,
// not the result: facts written before the rule still carry them, and they
// must stay editable, so an edit that keeps them or removes some passes. The
// error names the occurrences the edit added.
func CheckWikiLinkEdit(before, after string) error {
	was := WikiLinks(before)
	now := WikiLinks(after)
	if len(now) <= len(was) {
		return nil
	}
	// Name the added ones: an occurrence is added when the edit leaves more
	// copies of its text than the body had.
	left := make(map[string]int, len(was))
	for _, l := range was {
		left[l.Text]++
	}
	var added []WikiLink
	for _, l := range now {
		if left[l.Text] > 0 {
			left[l.Text]--
			continue
		}
		added = append(added, l)
	}
	return fmt.Errorf("%w: the edit adds %s, so the body would hold %d [[...]] outside code where it holds %d now. Ones the body already has may stay or be removed, but none may be added. %s",
		ErrWikiLinkInBody, listWikiLinks(added), len(now), len(was), wikiLinkAdvice)
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

// proseMask returns body with every byte of code — fenced blocks and inline
// code spans — replaced by a space, newlines kept, so offsets into the mask
// are offsets into body.
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
		paraStart = -1 // start of the current prose paragraph, -1 if none
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
		line := strings.TrimLeft(body[start:end], " \t")
		ch, n := fenceRun(line)
		switch {
		case fenceChar != 0:
			blank(start, end)
			if ch == fenceChar && n >= fenceLen && strings.TrimSpace(line[n:]) == "" {
				fenceChar = 0
			}
		case n > 0:
			endPara(start)
			blank(start, end)
			fenceChar, fenceLen = ch, n
		case strings.TrimSpace(line) == "":
			endPara(start)
		default:
			if paraStart < 0 {
				paraStart = start
			}
		}
		start = end + 1
	}
	endPara(len(body))
	return string(mask)
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
// offset base in the body.
func blankCodeSpans(para string, base int, blank func(from, to int)) {
	for i := 0; i < len(para); {
		if para[i] != '`' {
			i++
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

func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}
