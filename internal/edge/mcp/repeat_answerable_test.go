package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// RepeatAnswerable is the rule the chat gives the loop's no-progress guard.
//
// SABOTAGE: tool_effects.go RepeatAnswerable, delete the multiModeTools check
// -> the review and hypothesize rows fail; delete the ReplayNotWith loop -> the
// cursor rows fail; return true for an unknown tool -> that row fails.
func TestRepeatAnswerable(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args string
		want             bool
	}{
		{"query", "knomit_query", `{"text":"x"}`, true},
		{"explain", "knomit_explain", `{"file":"kb/a.md"}`, true},
		{"bind with identical args is idempotent", "knomit_bind", `{}`, true},
		{"an identical write must not run twice", "knomit_learn", `{"facts":[]}`, true},
		{"update", "knomit_update", `{"file":"a"}`, true},
		{"retract", "knomit_retract", `{"file":"a"}`, true},
		{"experiment commit", "knomit_experiment", `{"action":"commit"}`, true},
		{"query page cursor consumes a page", "knomit_query", `{"cursor":"c1"}`, false},
		{"explain cursor", "knomit_explain", `{"file":"a","cursor":"c1"}`, false},
		{"review page advances", "knomit_review", `{"session_id":"s","page":true}`, false},
		{"review start", "knomit_review", `{"start":true}`, false},
		{"hypothesize start", "knomit_hypothesize", `{"start":true}`, false},
		{"unknown tool", "knomit_nonesuch", `{}`, false},
		{"arguments that do not decode", "knomit_query", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, RepeatAnswerable(tc.tool, json.RawMessage(tc.args)))
		})
	}
}
