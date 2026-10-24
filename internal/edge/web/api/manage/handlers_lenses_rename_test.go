package manage

import (
	"errors"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"net/http"
	"testing"
)

// TestHandleLensRename_ErrStatusMapping pins the sentinel→(status,title)
// mapping directly, the same way TestLensCreateErrStatus does for create/
// patch. The ErrLensNotFound arm is the one deliberate divergence from
// renameErrStatus's repo-side mapping (409 there, 404 here) — see the
// comment on that arm for why. Named with the TestHandleLensRename_ prefix,
// not TestLensRenameErrStatus, so it runs under the same
// `-run TestHandleLensRename` filter as the HTTP-level tests above.
func TestHandleLensRename_ErrStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		{"invalid name", repos.ErrInvalidLensName, http.StatusBadRequest, "Invalid lens name"},
		{"name conflicts repo", repos.ErrLensNameConflictsRepo, http.StatusConflict, "Name already in use"},
		{"lens exists", control.ErrLensExists, http.StatusConflict, "Name already in use"},
		// Reachable only since RenameLens started reserving the target name in
		// the shared m.creating set: an in-flight Create/Restore/CreateLens
		// claiming it now surfaces as this sentinel instead of slipping past
		// the in-lock m.repos check. Same 409 and same wording as the repo
		// route's arm.
		{"create in flight", repos.ErrCreateInFlight, http.StatusConflict, "Name already in use"},
		{"lens not found", control.ErrLensNotFound, http.StatusNotFound, "Lens not found"},
		{"unmapped", errors.New("boom"), http.StatusInternalServerError, "Rename failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title, _ := lensRenameErrStatus(tc.err, "old", "new")
			if status != tc.wantStatus || title != tc.wantTitle {
				t.Errorf("got (%d, %q), want (%d, %q)", status, title, tc.wantStatus, tc.wantTitle)
			}
		})
	}
}
