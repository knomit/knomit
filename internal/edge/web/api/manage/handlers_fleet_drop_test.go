package manage

import (
	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/remotesync"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// #533 review 3 SF1: a refused drop's problem body carries leave_out, what
// the confirmed fallback would lose, by author and subject.
// SABOTAGE: drop the leave_out arm of writeFleetError -> red.
func TestFleetError_ADropRefusalCarriesLeaveOut(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFleetError(rec, httptest.NewRequest(http.MethodPost, "/fleet/drop-commit", nil), &repos.FleetError{
		Status: http.StatusConflict, Code: "foreign_commit", Msg: "m",
		LeaveOut: []remotesync.LostCommit{{Commit: strings.Repeat("a", 40), Agent: "alpha", Author: "alpha", Subject: "alpha work one"}},
	})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), `"leave_out":[{"commit":"`+strings.Repeat("a", 40)+`","agent":"alpha","author":"alpha","subject":"alpha work one"}]`)
}
