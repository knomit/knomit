package fact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func strp(s string) *string { return &s }

func replaceOp(old, new string) BodyOp {
	return BodyOp{Op: OpStrReplace, OldStr: old, NewStr: strp(new)}
}

func TestApplyBodyOps_SingleMatchReplaces(t *testing.T) {
	out, deltas, err := ApplyBodyOps("alpha beta gamma", []BodyOp{replaceOp("beta", "BETA!")})
	require.NoError(t, err)
	require.Equal(t, "alpha BETA! gamma", out)
	require.Equal(t, []BodyOpDelta{{Op: OpStrReplace, Delta: 1}}, deltas)
}

func TestApplyBodyOps_EmptyNewStrDeletes(t *testing.T) {
	out, deltas, err := ApplyBodyOps("keep. drop this. keep.", []BodyOp{replaceOp(" drop this.", "")})
	require.NoError(t, err)
	require.Equal(t, "keep. keep.", out)
	require.Equal(t, -11, deltas[0].Delta)
}

func TestApplyBodyOps_ZeroMatchesReportsPrefixOffsetAndDivergentChar(t *testing.T) {
	body := "intro line\nthe gate -- it rejects\n"
	_, _, err := ApplyBodyOps(body, []BodyOp{replaceOp("the gate — it rejects", "x")})
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "op 0: 0 matches")
	// Longest prefix that occurs is "the gate " (9 bytes) at offset 11.
	require.Contains(t, msg, "9 bytes")
	require.Contains(t, msg, "offset 11")
	require.Contains(t, msg, "U+2014")
	require.Contains(t, msg, "U+002D")
}

func TestApplyBodyOps_ZeroMatchesTrailingWhitespace(t *testing.T) {
	_, _, err := ApplyBodyOps("foo\nbar", []BodyOp{replaceOp("foo \nbar", "x")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "U+0020")
	require.Contains(t, err.Error(), "U+000A")
}

func TestApplyBodyOps_ZeroMatchesNoPrefixAtAll(t *testing.T) {
	_, _, err := ApplyBodyOps("abc", []BodyOp{replaceOp(" zzz", "x")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "op 0: 0 matches")
	require.Contains(t, err.Error(), "U+00A0")
}

func TestApplyBodyOps_ZeroMatchesPrefixRunsToEndOfBody(t *testing.T) {
	_, _, err := ApplyBodyOps("the end", []BodyOp{replaceOp("the end is near", "x")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "end of body")
}

func TestApplyBodyOps_MultipleMatchesReportsCountAndOffsets(t *testing.T) {
	_, _, err := ApplyBodyOps("x foo y foo z foo", []BodyOp{replaceOp("foo", "bar")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "op 0: 3 matches at offsets [2 8 14]")
}

func TestApplyBodyOps_MultipleMatchesCapsOffsetList(t *testing.T) {
	_, _, err := ApplyBodyOps(strings.Repeat("ab ", 50), []BodyOp{replaceOp("ab", "x")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "50 matches")
	require.Contains(t, err.Error(), "more")
	require.Less(t, len(err.Error()), 400, "the offset list must be capped")
}

// strings.Count counts non-overlapping occurrences, so "aa" in "aaa" reads as
// one match although it anchors at two places. Uniqueness must see both.
func TestApplyBodyOps_OverlappingMatchesAreAmbiguous(t *testing.T) {
	_, _, err := ApplyBodyOps("aaa", []BodyOp{replaceOp("aa", "b")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "2 matches at offsets [0 1]")
}

func TestApplyBodyOps_SequentialAnchoring(t *testing.T) {
	out, _, err := ApplyBodyOps("one two", []BodyOp{
		replaceOp("two", "two INSERTED"),
		replaceOp("INSERTED", "three"),
	})
	require.NoError(t, err)
	require.Equal(t, "one two three", out)
}

func TestApplyBodyOps_FailureNamesOpIndex(t *testing.T) {
	_, _, err := ApplyBodyOps("a b c", []BodyOp{
		replaceOp("a", "A"),
		replaceOp("b", "B"),
		replaceOp("missing", "x"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "op 2:")
}

func TestApplyBodyOps_Append(t *testing.T) {
	cases := []struct{ name, body, text, want string }{
		{"blank line inserted", "para one.", "para two.", "para one.\n\npara two."},
		{"one newline topped up", "para one.\n", "para two.", "para one.\n\npara two."},
		{"caller's leading newlines count", "para one.", "\n\npara two.", "para one.\n\npara two."},
		{"caller's trailing whitespace kept", "para one.", "para two.  \n", "para one.\n\npara two.  \n"},
		{"empty body", "", "first.", "first."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, deltas, err := ApplyBodyOps(c.body, []BodyOp{{Op: OpAppend, Text: c.text}})
			require.NoError(t, err)
			require.Equal(t, c.want, out)
			require.Equal(t, len(c.want)-len(c.body), deltas[0].Delta)
		})
	}
}

func TestApplyBodyOps_RejectsMalformedOps(t *testing.T) {
	cases := []struct {
		name string
		op   BodyOp
		want string
	}{
		{"unknown op", BodyOp{Op: "regex_replace"}, "unknown op"},
		{"empty old_str", BodyOp{Op: OpStrReplace, OldStr: "", NewStr: strp("x")}, "old_str"},
		{"missing new_str", BodyOp{Op: OpStrReplace, OldStr: "a"}, "new_str"},
		{"empty append text", BodyOp{Op: OpAppend}, "text"},
		{"append with old_str", BodyOp{Op: OpAppend, Text: "t", OldStr: "a"}, "old_str"},
		{"str_replace with text", BodyOp{Op: OpStrReplace, OldStr: "a", NewStr: strp("b"), Text: "t"}, "text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ApplyBodyOps("a", []BodyOp{c.op})
			require.Error(t, err)
			require.Contains(t, err.Error(), "op 0:")
			require.Contains(t, err.Error(), c.want)
		})
	}
}

func TestApplyBodyOps_RejectsEmptyList(t *testing.T) {
	_, _, err := ApplyBodyOps("a", nil)
	require.Error(t, err)
}
