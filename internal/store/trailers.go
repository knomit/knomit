package store

import (
	"context"
	"strings"
)

// Trailer keys of the causal trace (F07 revision 2, "Causal trace"). They are
// the last paragraph of a commit message, `Key: value` per line, inside the
// signed payload — so they cannot be altered without breaking the SSHSIG.
const (
	TrailerTrace   = "Knomit-Trace"   // which story the commit belongs to
	TrailerCause   = "Knomit-Cause"   // the one commit that directly led to this one
	TrailerTrigger = "Knomit-Trigger" // the trigger rule that made the hop
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
type Trailers struct {
	Trace   string // Knomit-Trace
	Cause   string // Knomit-Cause: the firing commit
	Trigger string // Knomit-Trigger: the trigger name
}

// IsZero reports whether no trailer is set.
func (t Trailers) IsZero() bool { return t.Trace == "" && t.Cause == "" && t.Trigger == "" }

type trailersCtxKey struct{}

// WithTrailers attaches a trailer set to ctx. Every authored commit the store
// builds under that ctx — writeFileExact, deleteFile and batchWrite, i.e. the
// three builders every WriteFact / WriteFactIfUnchanged / DeleteFact /
// BatchWriteFacts reaches — ends with the set as its last paragraph. Merge,
// replay and reconcile commits are built elsewhere and never see it: a
// transport commit carries no trailer and strips none.
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

// trailersFromContext is the read side of WithTrailers; a zero set when none.
func trailersFromContext(ctx context.Context) Trailers {
	t, _ := ctx.Value(trailersCtxKey{}).(Trailers)
	return t
}

// appendTrailers returns message with the non-empty keys of t appended as the
// LAST paragraph (git's trailer convention, and what TrailerValue reads: the
// last paragraph, last occurrence wins — so a body that happened to end with
// a `Knomit-Trace:` line is overridden by the stamped value, never the other
// way round). A zero set returns message unchanged; a key with an empty value
// is omitted.
func appendTrailers(message string, t Trailers) string {
	if t.IsZero() {
		return message
	}
	out := strings.TrimRight(message, "\n") + "\n\n"
	if t.Trace != "" {
		out += TrailerTrace + ": " + t.Trace + "\n"
	}
	if t.Cause != "" {
		out += TrailerCause + ": " + t.Cause + "\n"
	}
	if t.Trigger != "" {
		out += TrailerTrigger + ": " + t.Trigger + "\n"
	}
	return out
}
