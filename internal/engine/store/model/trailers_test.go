package model

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// AppendTrailers writes the set as the LAST paragraph, in the fixed key
// order, omitting empty keys; a zero set leaves the message alone; the stamped
// value wins over a body line of the same key (TrailerValue: last paragraph,
// last occurrence). Sabotage: append without the blank line (the body's
// last paragraph absorbs the keys and a body `Knomit-Trace:` line wins).
func TestAppendTrailers_LastParagraphAndReadBack(t *testing.T) {
	full := Trailers{Trace: "t-1", Cause: strings.Repeat("a", 40), Trigger: "inbox"}
	msg := AppendTrailers("learn: x\n\nbody para\n", full)
	require.Equal(t, "learn: x\n\nbody para\n\nKnomit-Trace: t-1\nKnomit-Cause: "+strings.Repeat("a", 40)+"\nKnomit-Trigger: inbox\n", msg)
	require.Equal(t, "t-1", TrailerValue(msg, TrailerTrace))
	require.Equal(t, strings.Repeat("a", 40), TrailerValue(msg, TrailerCause))
	require.Equal(t, "inbox", TrailerValue(msg, TrailerTrigger))

	require.Equal(t, "learn: x", AppendTrailers("learn: x", Trailers{}), "a zero set stamps nothing")
	require.Equal(t, "learn: x\n\nKnomit-Trigger: inbox\n", AppendTrailers("learn: x\n\n\n", Trailers{Trigger: "inbox"}), "empty keys are omitted; trailing newlines are normalised")

	// A body whose last line looks like a trailer: the stamped paragraph is
	// the LAST one, so the stamped value is what reads back.
	tricky := AppendTrailers("learn: x\n\nKnomit-Trace: body-says-so", Trailers{Trace: "stamped"})
	require.Equal(t, "stamped", TrailerValue(tricky, TrailerTrace))
}

// T9 Format: Run follows Trigger; the agent's own keys follow Run, sorted by
// key whatever order they were given in; a Run-only (or Extra-only) set is not
// zero; an empty Extra value is omitted. The F07 script paragraph (Trace,
// Cause, Trigger) is byte-identical to before. Sabotage: leave Run out of
// IsZero; swap Run and Trigger; stop sorting Extra.
func TestAppendTrailers_RunAndExtraOrder(t *testing.T) {
	cause := strings.Repeat("a", 40)
	run := "run-" + strings.Repeat("b", 32)
	full := Trailers{Trace: "t-1", Cause: cause, Trigger: "inbox", Run: run,
		Extra: []TrailerEntry{{Key: "Zeta", Value: "z"}, {Key: "Ticket", Value: "ABC-12"}, {Key: "Alpha", Value: "a"}}}
	require.Equal(t, "learn: x\n\nKnomit-Trace: t-1\nKnomit-Cause: "+cause+"\nKnomit-Trigger: inbox\nKnomit-Run: "+run+
		"\nAlpha: a\nTicket: ABC-12\nZeta: z\n", AppendTrailers("learn: x", full))
	require.Equal(t, []TrailerEntry{{Key: "Zeta", Value: "z"}, {Key: "Ticket", Value: "ABC-12"}, {Key: "Alpha", Value: "a"}}, full.Extra, "the caller's slice is not reordered")
	require.Equal(t, run, TrailerValue(AppendTrailers("learn: x", full), TrailerRun))
	require.Equal(t, "ABC-12", TrailerValue(AppendTrailers("learn: x", full), "ticket"))

	require.False(t, Trailers{Run: run}.IsZero(), "a Run-only set is a set")
	require.False(t, Trailers{Extra: []TrailerEntry{{Key: "Ticket", Value: "x"}}}.IsZero(), "an Extra-only set is a set")
	require.True(t, Trailers{Extra: []TrailerEntry{}}.IsZero())
	require.Equal(t, "learn: x\n\nKnomit-Run: "+run+"\n", AppendTrailers("learn: x", Trailers{Run: run}))
	require.Equal(t, "learn: x\n\nTicket: x\n", AppendTrailers("learn: x", Trailers{Extra: []TrailerEntry{{Key: "Empty", Value: ""}, {Key: "Ticket", Value: "x"}}}))

	// The F07 three-key paragraph is unchanged.
	require.Equal(t, "learn: x\n\nKnomit-Trace: t-1\nKnomit-Cause: "+cause+"\nKnomit-Trigger: inbox\n",
		AppendTrailers("learn: x", Trailers{Trace: "t-1", Cause: cause, Trigger: "inbox"}))
}

// F24 M3: a set holding ONLY the template keys is not zero, and renders them
// after Run, Template before TemplateSource. Sabotage: leave Template or
// TemplateSource out of IsZero — the create's commit would be unstamped and
// nothing else would fail.
func TestTrailers_TemplateOnlySetIsStamped(t *testing.T) {
	for _, set := range []Trailers{{Template: "mission"}, {TemplateSource: "kb://abc@def"}} {
		require.False(t, set.IsZero(), "%+v must not be zero", set)
	}
	set := Trailers{Template: "mission", TemplateSource: "kb://034f37d5b4a5@" + strings.Repeat("a", 40), Run: "run-x"}
	got := AppendTrailers("init: x", set)
	require.Equal(t, "init: x\n\nKnomit-Run: run-x\nKnomit-Template: mission\nKnomit-Template-Source: kb://034f37d5b4a5@"+strings.Repeat("a", 40)+"\n", got)
	require.Equal(t, "mission", TrailerValue(got, TrailerTemplate))

	// A ctx carrying a template set refuses an agent's trace on top of it,
	// exactly as it refuses one over a script's set.
	_, err := WithAgentTrace(WithTrailers(context.Background(), Trailers{Template: "m"}), Trailers{Trace: "t"})
	require.ErrorIs(t, err, ErrTraceOverride)
}

// ParseOutcome accepts only the two verdict words; any other word is refused.
//
// SABOTAGE: accept any word in ParseOutcome (ValidOutcome → true) → red.
func TestParseOutcome(t *testing.T) {
	for v, want := range map[string][3]any{
		"kb/a/b.md confirmed": {"kb/a/b.md", "confirmed", true},
		"kb/a/b.md refuted":   {"kb/a/b.md", "refuted", true},
		"kb/a b.md refuted":   {"kb/a b.md", "refuted", true},
		"kb/a/b.md maybe":     {"kb/a/b.md", "maybe", false},
		"kb/a/b.md":           {"", "", false},
		" refuted":            {"", "refuted", false},
		"kb/a/b.md CONFIRMED": {"kb/a/b.md", "CONFIRMED", false},
	} {
		p, o, ok := ParseOutcome(v)
		if ok != want[2].(bool) || (ok && (p != want[0] || o != want[1])) {
			t.Errorf("ParseOutcome(%q) = %q, %q, %v; want %v", v, p, o, ok, want)
		}
	}
}

// WithOutcomes keeps an existing trace set, appends one line per distinct pair,
// and the lines end the message in the last paragraph.
//
// SABOTAGE: drop the dedup or overwrite the existing set → red.
func TestWithOutcomes(t *testing.T) {
	ctx := WithTrailers(context.Background(), Trailers{Trace: "s"})
	ctx = WithOutcomes(ctx, []Outcome{{"kb/a.md", "confirmed"}, {"kb/a.md", "confirmed"}, {"kb/b.md", "refuted"}})
	got := AppendTrailers("learn: m", TrailersFromContext(ctx))
	want := "learn: m\n\nKnomit-Trace: s\nKnomit-Outcome: kb/a.md confirmed\nKnomit-Outcome: kb/b.md refuted\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !reflect.DeepEqual(TrailerValues(got, TrailerOutcome), []string{"kb/a.md confirmed", "kb/b.md refuted"}) {
		t.Fatal(TrailerValues(got, TrailerOutcome))
	}
	if WithOutcomes(context.Background(), nil) == nil {
		t.Fatal("nil ctx")
	}
}
