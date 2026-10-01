package mcp

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/client/sessions"
	"knomit/internal/store"
)

// The `trace` argument (#349): an agent attaches a few short `Key: value`
// entries to every commit one write call makes, so the writes of a task can be
// found later with `git log --grep='^Knomit-Trace: <id>'`. The entries travel
// with each call — never with the session or its connection — so a session
// working on several tasks, in turn or side by side, labels each write with
// its own task.
//
// The rules are the user's (R7): every `Knomit-` name is knomit's and reserved.
// An agent may pass only the three a recipe hands it — Knomit-Trace,
// Knomit-Cause and Knomit-Run, each in the exact form knomit produces — and
// never Knomit-Trigger (it switches the trigger loop guard) or an invented
// `Knomit-*` name. Any other key is a short git-trailer token with a short
// one-line value: entries carry context, not notes. A bad entry refuses the
// WHOLE call, naming the entry, before anything is written.

const (
	traceArgument = "trace"

	// MaxTraceKeyLen, MaxTraceValueLen and MaxTraceEntries bound an agent's
	// OWN entries (not the three Knomit- ones). Sanity bounds that keep the
	// entries what they are for — stamping and carrying context — not policy.
	MaxTraceKeyLen   = 32
	MaxTraceValueLen = 128
	MaxTraceEntries  = 8

	// reservedTracePrefix is matched in any case: git and TrailerValue match
	// trailer keys case-insensitively, so `knomit-trigger` IS Knomit-Trigger.
	reservedTracePrefix = "knomit-"
)

var (
	// traceKeyRE is git's trailer-key form — "ASCII alphanumeric characters
	// and hyphens" (git-interpret-trailers(1)) — with knomit's own rule that
	// it starts with an alphanumeric.
	traceKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	// traceCauseRE is a full commit hash as git prints it: lowercase, SHA-1
	// or SHA-256. `--grep` is case-sensitive, so an upper-case cause would
	// never be found by the hash git shows.
	traceCauseRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	// traceRunRE is the minted run id (newRunID): `run-` + 32 lowercase hex.
	traceRunRE = regexp.MustCompile(`^run-[0-9a-f]{32}$`)
)

// traceArgDescription is the agent-facing contract, written once for the five
// write tools.
const traceArgDescription = "Optional: short `Key: value` entries written at the end of every commit this call makes, " +
	"so the writes of one task can be found later (`git log --all --grep='^Knomit-Trace: <id>'`). " +
	"Pass the SAME object on every write for that task — knomit stores nothing between calls, so a write without it is untraced. " +
	"`Knomit-` names are knomit's: pass only Knomit-Trace, Knomit-Cause and Knomit-Run, exactly as you were given them " +
	"(Knomit-Trace: one line, at most 256 bytes; Knomit-Cause: a full lowercase commit hash; Knomit-Run: run- followed by 32 lowercase hex), " +
	"never Knomit-Trigger or any other Knomit- name. " +
	"Any other key: letters, digits and hyphens, starting with a letter or digit, at most 32 characters; " +
	"its value: one line, at most 128 bytes, no leading or trailing spaces; at most 8 such keys; " +
	"two keys differing only in case are refused. " +
	"Any entry outside these rules refuses the whole call and nothing is written. " +
	"Refused on a trigger script's or recipe's own write, which knomit stamps itself."

// traceArg is the `trace` schema property: an object of string values.
func traceArg() mcpgo.ToolOption {
	return mcpgo.WithObject(traceArgument,
		mcpgo.Description(traceArgDescription),
		mcpgo.AdditionalProperties(map[string]any{"type": "string"}),
	)
}

// applyTrace validates the call's `trace` and attaches it to ctx for every
// commit the call makes. Absent → ctx unchanged (D-mint: knomit stamps
// nothing an agent did not pass). Present on a ctx that already carries
// knomit's own set (a script or recipe write) → refused, even when empty. It
// runs before the handler writes anything, so a refusal writes nothing.
func applyTrace(ctx context.Context, req mcpgo.CallToolRequest) (context.Context, error) {
	raw, present := req.GetArguments()[traceArgument]
	if !present {
		return ctx, nil
	}
	t, err := parseTrace(raw)
	if err != nil {
		return ctx, err
	}
	return store.WithAgentTrace(ctx, t)
}

// parseTrace applies the rules above to the `trace` argument's value. Keys
// are checked in sorted order so the error for a call is deterministic.
func parseTrace(raw any) (store.Trailers, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return store.Trailers{}, fmt.Errorf("trace: must be an object of string values, got %T", raw)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var t store.Trailers
	seen := make(map[string]string, len(keys))
	for _, key := range keys {
		lower := strings.ToLower(key)
		if prev, dup := seen[lower]; dup {
			return store.Trailers{}, fmt.Errorf("trace: %q and %q are the same key (keys are matched case-insensitively)", prev, key)
		}
		seen[lower] = key

		value, ok := obj[key].(string)
		if !ok {
			return store.Trailers{}, fmt.Errorf("trace.%s: value must be a string, got %T", key, obj[key])
		}
		if strings.HasPrefix(lower, reservedTracePrefix) {
			if err := knomitTraceEntry(&t, key, lower, value); err != nil {
				return store.Trailers{}, err
			}
			continue
		}
		if len(key) > MaxTraceKeyLen {
			return store.Trailers{}, fmt.Errorf("trace: key %q is longer than %d characters", key, MaxTraceKeyLen)
		}
		if !traceKeyRE.MatchString(key) {
			return store.Trailers{}, fmt.Errorf("trace: key %q must be letters, digits and hyphens, starting with a letter or digit", key)
		}
		if err := traceValueRule(key, value, MaxTraceValueLen); err != nil {
			return store.Trailers{}, err
		}
		t.Extra = append(t.Extra, store.TrailerEntry{Key: key, Value: value})
	}
	if len(t.Extra) > MaxTraceEntries {
		return store.Trailers{}, fmt.Errorf("trace: %d entries of your own; at most %d (plus Knomit-Trace, Knomit-Cause and Knomit-Run)", len(t.Extra), MaxTraceEntries)
	}
	return t, nil
}

// knomitTraceEntry accepts one of the three supported Knomit- names in its
// exact form, written in knomit's canonical spelling; every other Knomit- name
// — Knomit-Trigger included — is refused.
func knomitTraceEntry(t *store.Trailers, key, lower, value string) error {
	switch lower {
	case strings.ToLower(store.TrailerTrace):
		if err := traceValueRule(key, value, sessions.MaxFieldLen); err != nil {
			return err
		}
		t.Trace = value
	case strings.ToLower(store.TrailerCause):
		if !traceCauseRE.MatchString(value) {
			return fmt.Errorf("trace.%s: must be a full commit hash in lowercase hex (40 or 64 characters)", key)
		}
		t.Cause = value
	case strings.ToLower(store.TrailerRun):
		if !traceRunRE.MatchString(value) {
			return fmt.Errorf("trace.%s: must be run- followed by 32 lowercase hex characters, as knomit mints it", key)
		}
		t.Run = value
	case strings.ToLower(store.TrailerTrigger):
		return fmt.Errorf("trace: %s is reserved: only knomit sets it (it switches the trigger loop guard)", key)
	default:
		return fmt.Errorf("trace: %s is reserved: names starting with Knomit- are knomit's; pass only Knomit-Trace, Knomit-Cause and Knomit-Run", key)
	}
	return nil
}

// traceValueRule: non-empty, at most max bytes, equal to its own TrimSpace
// (TrailerValue trims on read, so a padded value would miss a --grep), and
// one line.
func traceValueRule(key, value string, max int) error {
	switch {
	case value == "":
		return fmt.Errorf("trace.%s: value is empty", key)
	case len(value) > max:
		return fmt.Errorf("trace.%s: value is longer than %d bytes", key, max)
	case strings.TrimSpace(value) != value:
		return fmt.Errorf("trace.%s: value has leading or trailing whitespace", key)
	}
	if r, at, bad := multiLineRune(value); bad {
		return fmt.Errorf("trace.%s: value must be one line: %U at byte %d", key, r, at)
	}
	return nil
}

// multiLineRune finds the first character that would break a one-line value:
// invalid UTF-8, any character unicode.IsControl reports (C0 including CR and
// LF, DEL, C1 including U+0085), and the line and paragraph separators U+2028
// and U+2029.
func multiLineRune(s string) (rune, int, bool) {
	for i, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || r == ' ' || r == ' ' {
			return r, i, true
		}
	}
	return 0, 0, false
}

// checkMomentName refuses a moment_name that is not one line. It is copied
// into the commit message, so a line break in it could write any line —
// a `Knomit-Trigger:` paragraph included — past every rule above; with this
// check, `trace` is the only way an agent puts such a line in a commit.
func checkMomentName(s string) error {
	if r, at, bad := multiLineRune(s); bad {
		return fmt.Errorf("moment_name must be one line: %U at byte %d (use the trace argument for Key: value entries)", r, at)
	}
	return nil
}
