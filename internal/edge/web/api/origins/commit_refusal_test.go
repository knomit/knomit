package origins

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
)

// A connect refused for the reserved fleet id is a 422 the client can read,
// never the 500 of an unmapped error: guardSwap refuses it before anything
// changed. (The restore half is web's TestFleetReserved_RestoreStatus; both were
// one test before the split.)
// SABOTAGE: drop the ErrFleetOntologyReserved arm -> 500 -> red.
func TestFleetReserved_ConnectStatus(t *testing.T) {
	err := fmt.Errorf("register repo: %w", repos.ErrFleetOntologyReserved)
	require.True(t, isCommitRefusal(err), "a connect refused before anything changed")
	rec := httptest.NewRecorder()
	writeCommitProblem(rec, httptest.NewRequest(http.MethodPost, "/repos/kb/origin-sessions/x/commit", nil), hal.URLBuilder{}, "kb", err)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Fleet ontology reserved")
}
