package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// POST /fleet/drop-commit with leave_out_merge runs the confirmed fallback,
// which needs the confirmed list: without left_out it is 422
// leave_out_unconfirmed and nothing runs.
// SABOTAGE: route leave_out_merge to the plain drop -> 409 not_unpublished
// -> red.
func TestFleetREST_LeaveOutMergeNeedsTheConfirmedList(t *testing.T) {
	s, _ := fleetCreateServer(t, "127.0.0.1:0")
	require.Equal(t, http.StatusCreated, postFleetCreate(s, `{}`, true).Code)
	req := localPeer(httptest.NewRequest(http.MethodPost, "/fleet/drop-commit",
		strings.NewReader(`{"commit":"`+strings.Repeat("b", 40)+`","leave_out_merge":true}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"leave_out_unconfirmed"`)
}
