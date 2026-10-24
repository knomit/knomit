package manage

import (
	"fmt"
	"knomit/internal/corpus/repos"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fleet's refusal while the fleet repository is indexing names the
// Fleet page and carries no cancel-index link: that link was
// /repos//index:cancel, a generic route refused for the fleet repository
// anyway (#528 follow-up).
// SABOTAGE: drop the ErrIndexing arm of writeFleetError -> it falls to the
// lifecycle writer's 409 with the link -> red.
func TestFleetError_IndexingNamesTheFleetPageAndNoLink(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFleetError(rec, httptest.NewRequest(http.MethodPost, "/fleet/follow", nil), fmt.Errorf("attach: %w", repos.ErrIndexing))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"indexing"`)
	require.Contains(t, rec.Body.String(), "Fleet → Settings")
	require.NotContains(t, rec.Body.String(), "index:cancel")
	require.NotContains(t, rec.Body.String(), "_links")
}
