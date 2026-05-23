package claude

import (
	"sort"
	"strings"
	"testing"
)

func TestMatchIntents_TableDriven(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		prevRole string
		text     string
		want     []string // sorted "intent:rule"
	}{
		{
			name:     "correction_start_no",
			role:     "user",
			prevRole: "assistant",
			text:     "No, that's not what I meant.",
			want:     []string{"correction:phrase", "correction:start"},
		},
		{
			name:     "correction_actually",
			role:     "user",
			prevRole: "assistant",
			text:     "Actually, no, that's not right.",
			want:     []string{"correction:actually", "correction:phrase"},
		},
		{
			name:     "correction_false_start_no_need",
			role:     "user",
			prevRole: "assistant",
			text:     "No need to redo it.",
			want:     nil,
		},
		{
			name:     "correction_gated_off_when_prev_is_user",
			role:     "user",
			prevRole: "user",
			text:     "No, that's wrong.",
			want:     nil,
		},
		{
			name: "discovery_turns_out",
			role: "assistant",
			text: "Turns out the resolver walks the parent chain.",
			want: []string{"discovery:root"},
		},
		{
			name: "discovery_ah_i_see",
			role: "assistant",
			text: "Ah, I see — the vtab handles this differently.",
			want: []string{"discovery:ah"},
		},
		{
			name: "discovery_gated_off_for_user",
			role: "user",
			text: "Turns out you were right.",
			want: nil,
		},
		{
			name: "decision_lets_go_with",
			role: "assistant",
			text: "Let's go with the vtab approach.",
			want: []string{"decision:go-with"},
		},
		{
			name: "decision_role_agnostic_user",
			role: "user",
			text: "Let's pick the second option.",
			want: []string{"decision:go-with"},
		},
		{
			name: "fixbug_root_cause",
			role: "assistant",
			text: "The root cause was a missing vtab registration.",
			want: []string{"fix-bug:cause"},
		},
		{
			name: "fixbug_gated_off_for_user",
			role: "user",
			text: "The root cause is missing config.",
			want: nil,
		},
		{
			name: "gotcha_be_careful",
			role: "assistant",
			text: "Be careful — this only works if the branch is set.",
			want: []string{"gotcha:silent", "gotcha:warn"},
		},
		{
			name: "code_block_stripped",
			role: "assistant",
			text: "Here's an example:\n```go\n// be careful\n```\nMoving on.",
			want: nil,
		},
		{
			name: "empty_text_no_hits",
			role: "assistant",
			text: "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchIntents(tc.role, tc.text, tc.prevRole)
			labels := make([]string, len(got))
			for i, m := range got {
				labels[i] = m.intent + ":" + m.rule
			}
			sort.Strings(labels)
			if !equalStrings(labels, tc.want) {
				t.Errorf("matchIntents(%q,%q,prev=%q) labels = %v, want %v",
					tc.role, tc.text, tc.prevRole, labels, tc.want)
			}
		})
	}
}

func TestMatchIntents_QuoteExtraction(t *testing.T) {
	got := matchIntents("assistant", "Some preamble. The root cause was a missing vtab. Then more.", "")
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1", len(got))
	}
	if !strings.Contains(got[0].quote, "The root cause was a missing vtab") {
		t.Errorf("quote = %q, want substring 'The root cause was a missing vtab'", got[0].quote)
	}
	if strings.Contains(got[0].quote, "Some preamble") || strings.Contains(got[0].quote, "Then more") {
		t.Errorf("quote leaked neighbouring sentences: %q", got[0].quote)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
