package manage

import (
	"encoding/json"
	"fmt"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/fleetrec"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// F10: a member row shows every advertised field as the record has it, even
// someone else's odd values; addresses is always an array.
func TestFleetMemberRow(t *testing.T) {
	since := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	row := fleetMemberRow(repos.FleetMemberRow{Member: fleetrec.Member{Member: fact.Member{
		Agent: "peer", State: fact.MemberActive, Host: "h", Addresses: []string{"not-a-url"}, Git: "/git",
		Capabilities: map[string]string{"os": "plan9"},
	}, Path: "kb/members/peer/x.md"}, Since: &since})
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	require.JSONEq(t, `{"agent":"peer","state":"active","host":"h","branch":"","path":"kb/members/peer/x.md",
		"addresses":["not-a-url"],"git":"/git","capabilities":{"os":"plan9"},"since":"2026-10-15T12:00:00Z"}`, string(raw))

	raw, err = json.Marshal(fleetMemberRow(repos.FleetMemberRow{Member: fleetrec.Member{Member: fact.Member{Agent: "old", State: fact.MemberActive}}}))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, []any{}, m["addresses"], "an F09 record without addresses is []")
	require.Equal(t, map[string]any{}, m["capabilities"])
	require.NotContains(t, m, "since", "no since found: none shown")
}

// GET /fleet/members carries the master's last refusal of a member's branch
// on THAT member's row (matched by the record's branch) and nowhere else; a
// record with no branch never matches. An ENROLLED id (a leaf, no record, so
// no branch) carries the newest refusal whose tip its agent authored: a first
// join refused in flight (PR #527 SF3); a record without a branch does not.
// SABOTAGE: drop the last_refusal assignment in fleetMemberRows -> red; drop
// the enrolled arm -> d's row has none -> red; keep the oldest refusal by
// agent -> d's is "old" -> red.
func TestFleetMemberRows_LastRefusal(t *testing.T) {
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	refused := map[string]repos.PeerRefusal{
		"agent/b":  {Branch: "agent/b", Tip: "abc", Reason: "knomit: commit abc refused", At: at},
		"":         {Branch: "", Tip: "zzz", Reason: "never shown"},
		"agent/d":  {Branch: "agent/d", Tip: "def", Agent: "d", Reason: "no-cert", At: at.Add(time.Minute)},
		"agent/d2": {Branch: "agent/d2", Tip: "old", Agent: "d", Reason: "old", At: at},
		"agent/c":  {Branch: "agent/c", Tip: "ccc", Agent: "c", Reason: "c has a record", At: at},
	}
	rows := fleetMemberRows([]repos.FleetMemberRow{
		{Member: fleetrec.Member{Member: fact.Member{Agent: "a", State: fact.MemberActive, Branch: "agent/a"}}},
		{Member: fleetrec.Member{Member: fact.Member{Agent: "b", State: fact.MemberActive, Branch: "agent/b"}}},
		{Member: fleetrec.Member{Member: fact.Member{Agent: "c", State: fact.MemberActive}}},
		{Member: fleetrec.Member{Member: fact.Member{Agent: "d", State: repos.MemberEnrolled}}},
	}, refused)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got, 4)
	require.NotContains(t, got[0], "last_refusal")
	require.Equal(t, map[string]any{"branch": "agent/b", "tip": "abc", "reason": "knomit: commit abc refused", "at": "2026-10-15T12:00:00Z"}, got[1]["last_refusal"])
	require.NotContains(t, got[2], "last_refusal", "a record without a branch is never matched by agent")
	require.Equal(t, map[string]any{"branch": "agent/d", "tip": "def", "agent": "d", "reason": "no-cert", "at": "2026-10-15T12:01:00Z"}, got[3]["last_refusal"])
}

// A restore refused for the reserved fleet id is a 422 the client can read,
// never the 500 of an unmapped error: Identify refuses a restored fleet
// repository. (The connect half, guardSwap's refusal, is api/origins'
// TestFleetReserved_ConnectStatus.)
// SABOTAGE: drop the ErrFleetOntologyReserved arm -> 500 -> red.
func TestFleetReserved_RestoreStatus(t *testing.T) {
	err := fmt.Errorf("register repo: %w", repos.ErrFleetOntologyReserved)
	status, title := archiveErrStatus(err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "Fleet ontology reserved", title)
}
