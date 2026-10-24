package manage

import (
	"fmt"
	"knomit/internal/corpus/repos"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHandleRepoRename_LostRaceIs409 covers the finding parked from Task 4's
// review: once RenameRepo returns ErrRepoNotFound because a concurrent rename
// (or removal) won the race on oldName, the repo this request was addressed to
// plainly still exists — just under a different name — so it must not be
// reported as 404. That would tell the client "this name never existed" when
// the truth is "retry, someone else changed it out from under you".
//
// Constructing this without reaching into the manager's internals is not
// practical from the HTTP layer: it requires racing two RenameRepo calls
// against the same oldName and observing the loser, which is exactly what
// internal/corpus/repos/rename_test.go's
// TestRenameRepo_ConcurrentDifferentTargets_ExactlyOneWins already does at the
// manager layer. Rather than duplicate that race here with real goroutines
// (flaky, and not testing anything the handler owns), this drives the exact
// same error the loser observes — repos.ErrRepoNotFound wrapping oldName —
// straight into renameErrStatus and asserts the mapping directly.
func TestHandleRepoRename_LostRaceIs409(t *testing.T) {
	lostRace := fmt.Errorf("%w: %q", repos.ErrRepoNotFound, "alpha")
	status, title, _ := renameErrStatus(lostRace, "alpha", "beta")
	require.Equal(t, http.StatusConflict, status)
	require.NotContains(t, title, "not found",
		"the repo this request named plainly still exists; do not tell the client otherwise")
}

// TestHandleRepoRename_ManagerStoppedIs503 covers the shutdown-grace-window
// case: a request that lands in the 5s gap between Shutdown and the deferred
// Close finds the control.db tenants already nilled out, and RenameRepo
// reports that as repos.ErrManagerStopped. This is transient and retryable —
// the same class handleHALRepoPatch already answers 503 for on
// ErrRepoClosed/ErrStoreUnavailable — not a 500 implying the server is broken.
func TestHandleRepoRename_ManagerStoppedIs503(t *testing.T) {
	status, title, _ := renameErrStatus(repos.ErrManagerStopped, "alpha", "beta")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "Repo not ready", title)
}
