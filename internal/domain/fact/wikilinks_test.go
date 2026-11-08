package fact

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

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
}

func TestCheckWikiLinks_ListsAtMostThree(t *testing.T) {
	err := CheckWikiLinks("[[a]] [[b]] [[c]] [[d]] [[e]]")
	require.Error(t, err)
	require.Contains(t, err.Error(), "[[c]] (line 1), and 2 more")
	require.NotContains(t, err.Error(), "[[d]]")
}

func TestCheckWikiLinkEdit_NoMoreThanBefore(t *testing.T) {
	legacy := "old [[a1b2c3d4]] and [[slug]]"

	require.NoError(t, CheckWikiLinkEdit(legacy, legacy+"\n\nmore prose"), "keeping existing ones is allowed")
	require.NoError(t, CheckWikiLinkEdit(legacy, "old and [[slug]]"), "removing one is allowed")
	require.NoError(t, CheckWikiLinkEdit(legacy, "all gone"), "the cleanup is allowed")
	require.NoError(t, CheckWikiLinkEdit("a", "a\n\n```\n[[x]]\n```"), "adding one in code is allowed")

	err := CheckWikiLinkEdit(legacy, legacy+" plus [[new]]")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrWikiLinkInBody))
	require.Contains(t, err.Error(), "the edit adds [[new]] (line 1)")
	require.NotContains(t, err.Error(), "[[slug]] (line", "only the added occurrence is named")
	require.Contains(t, err.Error(), "so the body would hold 3 [[...]] outside code where it holds 2 now")
	require.Contains(t, err.Error(), "none may be added")
	require.Contains(t, err.Error(), "refs")

	err = CheckWikiLinkEdit("clean", "clean [[x]]")
	require.Error(t, err, "a body with none may not gain one")

	err = CheckWikiLinkEdit("[[a]]", "[[a]] [[a]]")
	require.Error(t, err, "a second copy of an existing one is an addition")
	require.Contains(t, err.Error(), "the edit adds [[a]]")
}
