package synthesize

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
)

// distillDeclineHealthLine renders the per-reason count of distill items that
// returned an empty synthesize. counts is keyed by declined_reason; "" is an
// item that declined without saying why. Nothing branches on this line —
// distill has no throttle and this work does not add one.
//
// "unstated" is rendered last and named rather than omitted: an item that
// declined without a reason is the case the reason field exists to surface, so
// hiding it would reproduce the invisibility this is meant to fix.
//
// ZERO DECLINES STILL RENDERS, without the parenthesised breakdown — there is
// none to show, and joining an empty list would print "0 of 9 items ()". A
// session that declined nothing SAYS so: silence used to mean any of three
// different things, and telling them apart is the point of this work.
func distillDeclineHealthLine(counts map[string]int, total int) string {
	declined := 0
	var keys []string
	for k, n := range counts {
		declined += n
		if k != "" {
			keys = append(keys, k)
		}
	}
	if declined == 0 {
		return fmt.Sprintf("distill declines: none of %d items", total)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	if n := counts[""]; n > 0 {
		parts = append(parts, fmt.Sprintf("unstated %d", n))
	}
	return fmt.Sprintf("distill declines: %d of %d items (%s)", declined, total, strings.Join(parts, ", "))
}

// distillDeclineHealth reads the session's answered distill responses and
// renders the decline line.
//
// It returns "" for EXACTLY ONE reason: the session had no distill items. A
// session that declined nothing says "none of N", and a failed read says
// "unavailable" — so an absent line carries one meaning rather than three, and
// the prune-only case is what pins it.
//
// It recomputes from the DURABLE record rather than from a running tally,
// because there is nowhere to keep one: sess.Health is in-memory and rides the
// FIRST result of a session, while declines only accrue as later turns answer
// items, and the engine is rebuilt per call so nothing in memory survives to
// completion. The response column is the only thing that outlives the turn
// that produced it.
//
// parseDistillResponse applies to the stored strings unchanged because the
// distill arm of Decode returns the agent's response as `normalized` verbatim —
// the response column holds the RAW text, not a re-marshalled form. This tally's
// correctness rests on that; if Decode ever normalises distill responses, this
// must parse whatever it stores instead.
//
// A parse failure is skipped rather than failing the session: this line is
// informational, the session's mutations are already committed, and a summary
// that errors would be strictly worse than one that undercounts.
func distillDeclineHealth(ctx context.Context, d Deps, sessionID string) string {
	responses, err := d.Pipeline.AnsweredDistillResponses(ctx, sessionID)
	if err != nil {
		// SAY the tally is unavailable rather than going quiet. A failed read
		// used to look exactly like a clean session, which is the one confusion
		// this line exists to prevent. Still fail-soft: the session's mutations
		// are committed and a summary must not error over a descriptor.
		log.Warn().Err(err).Str("session", sessionID).
			Msg("distill declines: could not read answered items; reporting unavailable")
		return "distill declines: unavailable (store read failed)"
	}
	if len(responses) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, raw := range responses {
		res, perr := parseDistillResponse(raw)
		if perr != nil {
			continue
		}
		if len(res.Synthesize) == 0 {
			counts[res.DeclinedReason]++
		}
	}
	// Rendered whenever the session HAD distill items, declines or not. An
	// absent line then means one thing — no distill work — instead of being
	// ambiguous between that, a clean session, and a failed read.
	return distillDeclineHealthLine(counts, len(responses))
}
