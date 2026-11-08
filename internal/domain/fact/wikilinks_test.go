package fact

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// approvedWikiLinkTail is the shared tail of both refusals as the user
// approved it (2026-11-08).
const approvedWikiLinkTail = "knomit has no link syntax in a body: it neither resolves nor renders [[...]], and every reference, to another fact or to an external source, goes in refs. Put the cited fact's path (kb/<topic>/<category>/<id>.md, as knomit_query returned it) in refs, and keep the body plain text. To show [[...]] literally, as in TOML or a regex, put it in an inline code span or a fenced code block."

func wikiTexts(body string) []string {
	var out []string
	for _, l := range WikiLinks(body) {
		out = append(out, l.Text)
	}
	return out
}

func TestWikiLinks_ProseCountsCodeIsExempt(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"prose id", "see [[f6db3a49]] for why", []string{"[[f6db3a49]]"}},
		{"prose path and slug", "[[kb/x/y/abcd1234]] and [[some-slug]]", []string{"[[kb/x/y/abcd1234]]", "[[some-slug]]"}},
		{"backtick fence", "intro\n\n```toml\n[[oauth.client]]\n```\n\nafter", nil},
		{"tilde fence", "~~~\n[[x]]\n~~~", nil},
		{"indented fence in a list", "- item\n  ```\n  [[x]]\n  ```", nil},
		{"longer fence closes only on a run as long", "````\n```\n[[x]]\n````\n[[y]]", []string{"[[y]]"}},
		{"unclosed fence runs to the end", "a\n```\n[[x]]", nil},
		{"inline span", "use `[[:space:]]` in the regex", nil},
		{"double backtick span", "the header ``[[a]]`` here", nil},
		{"span across a line", "a `x\n[[y]]` b", nil},
		{"unmatched backtick is literal", "a ` then [[y]]", []string{"[[y]]"}},
		{"span does not cross a paragraph", "a `x\n\n[[y]] b`", []string{"[[y]]"}},
		{"inline triple backtick line is a span, not a fence", "```x``` then\n[[y]]", []string{"[[y]]"}},
		{"after a fence closes, prose counts again", "```\ncode\n```\n[[z]]", []string{"[[z]]"}},
		{"lone open bracket pair", "an array a[[0]: no close", nil},
		{"lone open with close on another line", "[[a\nb]]", nil},
		{"blank inside", "[[  ]] is not one", nil},
		{"single brackets", "[x] and [text](kb/a.md)", nil},
		// F2: a fence opened on a list-marker or blockquote line.
		{"fence on a dash list line", "- ```toml\n  [[oauth.client]]\n  ```\n\nprose [[z]]", []string{"[[z]]"}},
		{"fence on a numbered list line", "1. ```\n   [[a]]\n   ```\n\n2) ```\n   [[b]]\n   ```\n[[z]]", []string{"[[z]]"}},
		{"fence in a blockquote", "> ~~~\n> [[a]]\n> ~~~\n\n[[z]]", []string{"[[z]]"}},
		{"fence in a quoted list", "> - ~~~\n>   [[a]]\n>   ~~~\n\n[[z]]", []string{"[[z]]"}},
		{"list item prose still counts", "- see [[a]]\n* and [[b]]\n+ and [[c]]\n3. and [[d]]", []string{"[[a]]", "[[b]]", "[[c]]", "[[d]]"}},
		{"quoted prose still counts", "> see [[a]]", []string{"[[a]]"}},
		{"bold is not a list marker", "**[[a]]**", []string{"[[a]]"}},
		// F6: indented code blocks.
		{"indented block after a blank line", "intro\n\n    [[x]] = 1\n\nafter [[y]]", []string{"[[y]]"}},
		{"indented block at the start", "    [[x]] = 1", nil},
		{"tab-indented block", "intro\n\n\t[[x]] = 1", nil},
		{"indented block across a blank line", "    [[x]]\n\n    [[y]]\nz [[z]]", []string{"[[z]]"}},
		{"indented line continues a paragraph", "text\n    [[x]]", []string{"[[x]]"}},
		{"indented line continues a list item", "- item\n\n    [[x]]", []string{"[[x]]"}},
		{"after a list ends, indented is code again", "- item\n\nback\n\n    [[x]]", nil},
		// F6: escaped backticks.
		{"escaped backtick does not open", "a \\`[[x]]` b", []string{"[[x]]"}},
		{"escaped backticks around it", "\\`[[x]]\\`", []string{"[[x]]"}},
		{"double backslash does not escape", "a \\\\`[[x]]` b", nil},
		{"span after an escaped backtick", "a \\` b `[[y]]`", nil},
		// F6: a span does not cross a heading line.
		{"span does not cross a heading", "a `x\n# Head\n[[y]] `", []string{"[[y]]"}},
		{"span inside a heading", "# The `[[a]]` header", nil},
		// F6: nested brackets.
		{"nested single brackets", "see [[the [x] fact]] here", []string{"[[the [x] fact]]"}},
		{"two on one line", "[[a]] then [[b]]", []string{"[[a]]", "[[b]]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, wikiTexts(tc.body))
		})
	}
}

func TestWikiLinks_LineNumbers(t *testing.T) {
	links := WikiLinks("one\n```\n[[code]]\n```\nfive [[a]]\n\nseven [[b]]")
	require.Equal(t, []WikiLink{{Text: "[[a]]", Line: 5}, {Text: "[[b]]", Line: 7}}, links)
}

func TestCheckWikiLinks_ErrorSaysWhatToSend(t *testing.T) {
	require.NoError(t, CheckWikiLinks("plain\n\n```\n[[ok]]\n```\n`[[ok]]`"))

	err := CheckWikiLinks("x\nsee [[f6db3a49]]")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrWikiLinkInBody))
	msg := err.Error()
	require.Contains(t, msg, "[[f6db3a49]] (line 2)")
	require.Contains(t, msg, "refs", "the error must name the channel to use instead")
	require.Contains(t, msg, "kb/<topic>/<category>/<id>.md")
	require.Contains(t, msg, "keep the body plain text")
	require.Contains(t, msg, "inline code span or a fenced code block", "the error must name the code exemption")
	require.Equal(t, "wiki link in fact body: the body contains [[f6db3a49]] (line 2). "+approvedWikiLinkTail, msg,
		"the approved wording (2026-11-08), byte for byte")
	require.Contains(t, msg, "external source")
}

func TestCheckWikiLinks_ListsAtMostThree(t *testing.T) {
	err := CheckWikiLinks("[[a]] [[b]] [[c]] [[d]] [[e]]")
	require.Error(t, err)
	require.Contains(t, err.Error(), "[[c]] (line 1), and 2 more")
	require.NotContains(t, err.Error(), "[[d]]")
}

// The update rule judges the resulting body only: there is no "before".
func TestCheckWikiLinksUpdate_ResultingBodyOnly(t *testing.T) {
	require.NoError(t, CheckWikiLinksUpdate("all gone"))
	require.NoError(t, CheckWikiLinksUpdate("code is fine:\n\n```\n[[x]]\n```\n\nand `[[y]]`"))

	err := CheckWikiLinksUpdate("one\ntwo\nold [[old-one]] kept")
	require.True(t, errors.Is(err, ErrWikiLinkInBody))
	require.Equal(t, "wiki link in fact body: the body contains [[old-one]] (line 3). An update must leave no [[...]] outside code, including ones the fact already had. "+approvedWikiLinkTail,
		err.Error(), "the drafted update wording, byte for byte")
	require.Contains(t, err.Error(), "external source")
}
