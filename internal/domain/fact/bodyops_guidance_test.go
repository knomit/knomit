package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A live chat run sent a
// str_replace op carrying `text`, got "op 0: str_replace does not take text"
// three times with byte-identical arguments, and the repeat-failure guard
// ended the turn. The error said what was wrong but not what to send, so the
// model had nothing to change. These cases pin that a cross-op field error
// names the right fields for the op it was sent with, and the other op's
// shape, whichever shape the model sent.
func TestApplyBodyOps_CrossOpFieldErrorsSayWhatToSend(t *testing.T) {
	for _, c := range []struct {
		name string
		op   BodyOp
		want []string
	}{
		// text in place of old_str/new_str: the likeliest live shape.
		{"str_replace with text only", BodyOp{Op: OpStrReplace, Text: "new paragraph"},
			[]string{"old_str", "new_str", "append"}},
		// all three fields: the text check fires first, so the same error.
		{"str_replace with text and anchors", BodyOp{Op: OpStrReplace, OldStr: "a", NewStr: strp("b"), Text: "t"},
			[]string{"old_str", "new_str", "append"}},
		// the next error a text-only str_replace caller hits once text is
		// dropped: it must say what old_str has to be.
		{"str_replace with empty old_str", BodyOp{Op: OpStrReplace, NewStr: strp("b")},
			[]string{"old_str", "copied from the body", "exactly once"}},
		{"append with old_str", BodyOp{Op: OpAppend, Text: "t", OldStr: "a"},
			[]string{"text", "str_replace"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ApplyBodyOps("a body", []BodyOp{c.op})
			require.Error(t, err)
			for _, w := range c.want {
				require.Contains(t, err.Error(), w, "the error must steer the caller to the right argument shape")
			}
		})
	}
}
