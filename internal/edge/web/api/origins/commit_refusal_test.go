package origins

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/foundation/pki"
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

// Every guard refusal that arrives before the stream opens is an HTTP problem
// with a status that says whose fault it is: the client's input or the
// incoming store's content is a 422, a failure of ours (an unreadable incoming
// store) stays a 500, and the specific mappings are unchanged.
// SABOTAGE: drop the 422 case -> ErrPeerCredential and friends read 500 -> red.
func TestWriteCommitProblem_RefusalStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"peer credential", repos.Refused(repos.ErrPeerCredential), http.StatusUnprocessableEntity},
		{"cert not an input", repos.Refused(repos.ErrCertNotAnInput), http.StatusUnprocessableEntity},
		{"unsupported scheme", repos.Refused(fmt.Errorf("probe: %w", pki.ErrUnsupportedOriginScheme)), http.StatusUnprocessableEntity},
		{"symlinked ontology", repos.Refused(fmt.Errorf("the incoming store's ontology: %w; connect aborted", knomitfact.ErrSymlinkNotFollowed)), http.StatusUnprocessableEntity},
		{"identity read", repos.Refused(fmt.Errorf("read the incoming store's identity: %w", errors.New("open: disk"))), http.StatusInternalServerError},
		{"already local", repos.Refused(repos.ErrKnowledgeBaseAlreadyLocal), http.StatusConflict},
		{"fleet reserved", repos.Refused(repos.ErrFleetOntologyReserved), http.StatusUnprocessableEntity},
		{"registry unavailable", repos.Refused(repos.ErrRegistryUnavailable), http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.True(t, isCommitRefusal(c.err))
			rec := httptest.NewRecorder()
			writeCommitProblem(rec, httptest.NewRequest(http.MethodPost, "/repos/kb/origin-sessions/x/commit", nil), hal.URLBuilder{}, "kb", c.err)
			require.Equal(t, c.want, rec.Code, rec.Body.String())
		})
	}
}
