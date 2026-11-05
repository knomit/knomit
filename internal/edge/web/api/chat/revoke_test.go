package chat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Revoking "Allow for session": DELETE /chats/{id}/grants names one grant as
// the listing shows it, and only that grant goes.

func grantsOf(t *testing.T, h *harness, who, id string) []grantInfo {
	t.Helper()
	code, body := h.do(who, http.MethodGet, "/chats/"+id+"/grants", nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var v struct {
		Embedded struct {
			Grants []grantInfo `json:"grants"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	return v.Embedded.Grants
}

// SABOTAGE: chat.go revokeGrant, make it return true without deleting -> the
// second turn's alpha write is not asked and the test fails.
func TestChat_RevokedGrantAsksAgainAndLeavesTheOtherInForce(t *testing.T) {
	h := newHarness(t)
	a := h.bind(writer, "chat-00000000000000000000000000000051", "alpha")
	b := h.bind(writer, "chat-00000000000000000000000000000052", "beta")
	h.queue("a1",
		toolStep("tu_1", "knomit_learn", learnArgs(a, "Alpha one"), usage),
		toolStep("tu_2", "knomit_learn", learnArgs(b, "Beta one"), usage),
		textStep("first done", usage),
		toolStep("tu_3", "knomit_learn", learnArgs(a, "Alpha two"), usage),
		toolStep("tu_4", "knomit_learn", learnArgs(b, "Beta two"), usage),
		textStep("second done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "one"))
	_, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_1": "allow_session", "tu_2": "allow_session"})
	require.Equal(t, []string{"tu_1", "tu_2"}, approvedIDs(asked))
	require.Equal(t, []grantInfo{
		{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"},
		{Target: "beta", Kind: "repo", Tool: "knomit_learn", Action: "learn"},
	}, grantsOf(t, h, "uid:501", id))

	// Revoke alpha's grant only.
	code, body := h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants",
		grantInfo{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"})
	require.Equal(t, http.StatusNoContent, code, string(body))
	require.Equal(t, []grantInfo{{Target: "beta", Kind: "repo", Tool: "knomit_learn", Action: "learn"}}, grantsOf(t, h, "uid:501", id))

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "two"))
	_, asked = drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, []string{"tu_3"}, approvedIDs(asked), "alpha asks again; beta's grant is still in force")
	require.Equal(t, "alpha", asked[0].Target)
}

func TestChat_RevokeOfAnUnknownGrantIs404(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000053", "alpha")
	h.queue("a1", toolStep("tu_1", "knomit_learn", learnArgs(handle, "Alpha"), usage), textStep("ok", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	drive(t, h, "uid:501", id, s, map[string]string{"tu_1": "allow_session"})

	for name, g := range map[string]grantInfo{
		"another target": {Target: "beta", Kind: "repo", Tool: "knomit_learn", Action: "learn"},
		"another action": {Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "update"},
		"another tool":   {Target: "alpha", Kind: "repo", Tool: "knomit_update", Action: "learn"},
		"another kind":   {Target: "alpha", Kind: "lens", Tool: "knomit_learn", Action: "learn"},
	} {
		code, _ := h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", g)
		require.Equal(t, http.StatusNotFound, code, name)
	}
	require.Len(t, grantsOf(t, h, "uid:501", id), 1, "none of those removed the real grant")

	// A second revoke of the same grant: the first removed it.
	real := grantInfo{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"}
	code, _ := h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", real)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", real)
	require.Equal(t, http.StatusNotFound, code)

	// An incomplete body is a bad request, not a 404.
	code, _ = h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", grantInfo{Target: "alpha"})
	require.Equal(t, http.StatusBadRequest, code)
}

// Ownership is the approvals route's: another principal's chat is a 404, and
// their revoke changes nothing.
func TestChat_RevokeIsTheOwnersAlone(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000054", "alpha")
	h.queue("a1", toolStep("tu_1", "knomit_learn", learnArgs(handle, "Alpha"), usage), textStep("ok", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	drive(t, h, "uid:501", id, s, map[string]string{"tu_1": "allow_session"})

	g := grantInfo{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"}
	code, _ := h.do("uid:1000", http.MethodDelete, "/chats/"+id+"/grants", g)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = h.do("", http.MethodDelete, "/chats/"+id+"/grants", g)
	require.Equal(t, http.StatusUnauthorized, code)
	require.Len(t, grantsOf(t, h, "uid:501", id), 1)
}

// The usage event numbers the chat's requests, so a client can tell a repeat of
// one request's usage from the next request's.
//
// SABOTAGE: chat.go onLoopEvent, drop `c.usageSeq++` -> every seq is 0 and the
// test fails.
func TestChat_UsageEventsCarryAnIncreasingSeq(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", toolStep("tu_r", "knomit_repos", map[string]any{}, usage), textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	var seqs []int
	for _, e := range evs {
		if e.Name != "usage" {
			continue
		}
		var u struct {
			Seq int `json:"seq"`
		}
		require.NoError(t, json.Unmarshal(e.Data, &u))
		seqs = append(seqs, u.Seq)
	}
	require.Equal(t, []int{1, 2}, seqs, "two requests: the tool call and the answer")
}
