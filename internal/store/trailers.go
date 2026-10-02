package store

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// Trailer keys of the causal trace (F07 revision 2, "Causal trace"). They are
// the last paragraph of a commit message, `Key: value` per line, inside the
// signed payload — so they cannot be altered without breaking the SSHSIG.
const (
	TrailerTrace   = "Knomit-Trace"   // which story the commit belongs to
	TrailerCause   = "Knomit-Cause"   // the one commit that directly led to this one
	TrailerTrigger = "Knomit-Trigger" // the trigger rule that made the hop
	TrailerRun     = "Knomit-Run"     // the recipe run (run-<32 hex>) the write belongs to (#349)
)

// TrailerValue returns the value of the `Key: value` trailer line in a commit
// message, or "" when absent. go-git has no trailer parser and knomit's
// commit_log keeps only the first line, so this reads the message itself:
// trailers are the `Key: value` lines of the LAST paragraph, and the last
// occurrence of the key wins (git's own convention). The key is matched
// case-insensitively.
//
// The dispatcher reads the three keys above from the firing commit; a script's
// writes are stamped with them through WithTrailers (F07 PR 3). The value is
// returned trimmed, never parsed further.
func TrailerValue(message, key string) string {
	msg := strings.TrimRight(message, "\n")
	// The last paragraph: everything after the final blank line. A message
	// with a single paragraph is its own last paragraph, so a trailer written
	// straight under the subject line still reads.
	last := msg
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 {
		last = msg[i+2:]
	}
	value, found := "", false
	for _, line := range strings.Split(last, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		value, found = strings.TrimSpace(v), true
	}
	if !found {
		return ""
	}
	return value
}

// Trailers is the causal-trace set a writer may attach to its commits.
//
// knomit sets Trace, Cause and Trigger on a trigger script's writes, and Run
// as well on a recipe's own writes (#349). An AGENT passes its set per call
// through the MCP `trace` argument (WithAgentTrace): Trace, Cause and Run in
// knomit's own forms — never Trigger — plus Extra, its own short keys.
type Trailers struct {
	Trace   string         // Knomit-Trace
	Cause   string         // Knomit-Cause: the firing commit
	Trigger string         // Knomit-Trigger: the trigger name
	Run     string         // Knomit-Run: the recipe run id
	Extra   []TrailerEntry // an agent's own keys (never Knomit-*); written after Run, sorted by key
}

// TrailerEntry is one agent-supplied `Key: value` line. internal/mcp
// validates both halves (trace.go); the store writes them as given.
type TrailerEntry struct {
	Key   string
	Value string
}

// IsZero reports whether no trailer is set.
func (t Trailers) IsZero() bool {
	return t.Trace == "" && t.Cause == "" && t.Trigger == "" && t.Run == "" && len(t.Extra) == 0
}

type trailersCtxKey struct{}

// WithTrailers attaches a trailer set to ctx. Every authored commit the store
// builds under that ctx — writeFileExact, deleteFile and batchWrite, i.e. the
// three builders every WriteFact / WriteFactIfUnchanged / DeleteFact /
// BatchWriteFacts reaches — ends with the set as its last paragraph. Merge,
// replay and reconcile commits are built elsewhere and never see it: a
// transport commit carries no trailer and strips none. The ONE exception is
// an experiment's own merge (CommitExperiment, SyncExperiment), which opts the
// ctx set in explicitly through mergeOpts.trace (appendTrailersToParagraph).
//
// The transport is a ctx value (precedent: WithPrecomputedEmbeddings) so the
// four FactIndex write methods and their callers keep their signatures; the
// MCP handlers pass their ctx to the write, which is how a script host stamps
// a write it makes through an UNCHANGED handler. A caller that sets nothing
// gets no paragraph. The value survives context.WithoutCancel (notifyCommit
// drops cancellation, not values).
func WithTrailers(ctx context.Context, t Trailers) context.Context {
	return context.WithValue(ctx, trailersCtxKey{}, t)
}

// ErrTraceOverride refuses an agent's trace on a write whose ctx already
// carries knomit's own set (a trigger script's or recipe's write): knomit's
// entries are never overridden, and agent entries are never merged into them.
var ErrTraceOverride = errors.New("trace: this write already carries knomit's own trace entries (a trigger script or recipe write); an agent trace cannot be added to them")

// ErrTraceTrigger refuses Knomit-Trigger in an agent's set: it switches the
// trigger loop guard, so only knomit sets it.
var ErrTraceTrigger = errors.New("trace: " + TrailerTrigger + " is reserved: only knomit sets it")

// WithAgentTrace attaches an AGENT's set (the MCP `trace` argument, already
// validated) to ctx, so every authored commit the call makes ends with it. It
// REFUSES — never merges, never overwrites — when ctx already carries a set:
// that is how a script's or recipe's write keeps knomit's entries (#349) — and
// it refuses so even for a zero set, because the caller calls it only when an
// agent PASSED a trace, and a passed trace is refused, never dropped. A zero
// set on a ctx with none returns ctx unchanged: knomit stamps nothing an agent
// did not pass (D-mint).
func WithAgentTrace(ctx context.Context, t Trailers) (context.Context, error) {
	if _, set := ctx.Value(trailersCtxKey{}).(Trailers); set {
		return ctx, ErrTraceOverride
	}
	if t.Trigger != "" {
		return ctx, ErrTraceTrigger
	}
	if t.IsZero() {
		return ctx, nil
	}
	return WithTrailers(ctx, t), nil
}

// trailersFromContext is the read side of WithTrailers; a zero set when none.
func trailersFromContext(ctx context.Context) Trailers {
	t, _ := ctx.Value(trailersCtxKey{}).(Trailers)
	return t
}

// appendTrailers returns message with the non-empty keys of t appended as the
// LAST paragraph (git's trailer convention, and what TrailerValue reads: the
// last paragraph, last occurrence wins — so a body that happened to end with
// a `Knomit-Trace:` line is overridden by the stamped value, never the other
// way round). The order is fixed: Trace, Cause, Trigger, Run, then the Extra
// entries sorted by key. A zero set returns message unchanged; a key with an
// empty value is omitted.
func appendTrailers(message string, t Trailers) string {
	if t.IsZero() {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + t.lines()
}

// lines renders the non-empty keys of t, one `Key: value` line each, in the
// fixed order appendTrailers documents.
func (t Trailers) lines() string {
	var out string
	if t.Trace != "" {
		out += TrailerTrace + ": " + t.Trace + "\n"
	}
	if t.Cause != "" {
		out += TrailerCause + ": " + t.Cause + "\n"
	}
	if t.Trigger != "" {
		out += TrailerTrigger + ": " + t.Trigger + "\n"
	}
	if t.Run != "" {
		out += TrailerRun + ": " + t.Run + "\n"
	}
	extra := append([]TrailerEntry(nil), t.Extra...)
	sort.SliceStable(extra, func(i, j int) bool { return extra[i].Key < extra[j].Key })
	for _, e := range extra {
		if e.Value != "" {
			out += e.Key + ": " + e.Value + "\n"
		}
	}
	return out
}

// appendTrailersToParagraph is appendTrailers for a message that may ALREADY
// end with a trailer paragraph — a merge commit's conflict record (key per
// path, appendTrailerLines). The trace lines join THAT paragraph, after the
// record, instead of opening a new one: both TrailerValue (Knomit-Trace) and
// TrailerValues (the conflict record) read only the LAST paragraph, so a
// second paragraph would hide the record from its readers. With no trailer
// paragraph it is appendTrailers.
//
// Used only by an experiment's own merges (CommitExperiment, SyncExperiment),
// which opt in through mergeOpts.trace; every other merge stays unstamped.
func appendTrailersToParagraph(message string, t Trailers) string {
	if t.IsZero() {
		return message
	}
	msg := strings.TrimRight(message, "\n")
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 && isTrailerParagraph(msg[i+2:]) {
		return msg + "\n" + t.lines()
	}
	return appendTrailers(message, t)
}
