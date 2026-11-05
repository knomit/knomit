package chat

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// A side-effecting call whose target cannot be resolved (no `binding`, or a
// handle that names nothing) is never run unprompted: it asks, it cannot be
// granted, and declining it means it did not run. Live acceptance (PR #763)
// saw an unbound knomit_learn run with decision "auto" and no card.
//
// SABOTAGE: chat.go Decide, return Allow/"auto" when !pinOK -> every case
// fails (no approval event arrives).
func TestChat_UnresolvedTargetWriteAsksAndIsNotGrantable(t *testing.T) {
	cases := []struct {
		name, tool string
		args       map[string]any
		action     string
	}{
		{"learn", "knomit_learn", map[string]any{"moment_name": "m", "facts": []any{}}, "learn"},
		{"update", "knomit_update", map[string]any{"path": "kb/x/y/z.md"}, "update"},
		{"retract", "knomit_retract", map[string]any{"path": "kb/x/y/z.md"}, "retract"},
		{"experiment open", "knomit_experiment", map[string]any{"action": "open", "name": "e"}, "open"},
		{"experiment sync", "knomit_experiment", map[string]any{"action": "sync"}, "sync"},
		{"experiment commit", "knomit_experiment", map[string]any{"action": "commit"}, "commit"},
		{"experiment rollback", "knomit_experiment", map[string]any{"action": "rollback"}, "rollback"},
		{"review start", "knomit_review", map[string]any{"mode": "start"}, "start"},
		{"review answer", "knomit_review", map[string]any{"session_id": "s", "response": "r"}, "answer"},
		{"hypothesize start", "knomit_hypothesize", map[string]any{}, "start"},
	}
	for _, tc := range cases {
		for _, variant := range []string{"absent", "bogus"} {
			t.Run(tc.name+"/"+variant, func(t *testing.T) {
				h := newHarness(t)
				args := map[string]any{}
				for k, v := range tc.args {
					args[k] = v
				}
				if variant == "bogus" {
					args["binding"] = "not-a-handle"
				}
				h.queue("a1", toolStep("tu_x1", tc.tool, args, usage), textStep("done", usage))
				id := h.create("uid:501", "a1")
				s := h.attach("uid:501", id)
				require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
				evs, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_x1": "decline"})

				require.Equal(t, []string{"tu_x1"}, approvedIDs(asked), "the unresolved write asked: %v", names(evs))
				require.False(t, asked[0].Grantable, "an unresolved target is never grantable")
				require.Equal(t, tc.action, asked[0].Action)
				require.Equal(t, llm.DeclinedResult, callResult(t, h, "uid:501", id, "tu_x1").Result, "the declined call did not run")
			})
		}
	}
}
