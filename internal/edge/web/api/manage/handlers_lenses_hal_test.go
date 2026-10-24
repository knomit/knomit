package manage

import (
	"errors"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"net/http"
	"testing"
)

// TestLensCreateErrStatus pins the sentinel→(status,title) mapping. The
// ErrCreateInFlight arm is the FIX-1 case: a concurrent same-name create (or a
// lens create racing a repo create for the same name) must map to 409, not the
// 500 default. This mapper is the only deterministic way to exercise that arm —
// the in-flight reservation seam (reserveNameAndOrigin) is unexported.
func TestLensCreateErrStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		{"invalid name", repos.ErrInvalidLensName, http.StatusBadRequest, "Invalid lens name"},
		{"name conflicts repo", repos.ErrLensNameConflictsRepo, http.StatusConflict, "Lens name conflicts with a repo"},
		{"lens exists", control.ErrLensExists, http.StatusConflict, "Lens already exists"},
		{"create in flight", repos.ErrCreateInFlight, http.StatusConflict, "Create in flight"},
		{"replica in lens", repos.ErrReplicaInLens, http.StatusConflict, "Replica mounts not allowed"},
		{"repo not found", repos.ErrRepoNotFound, http.StatusUnprocessableEntity, "Lens references an unknown repo"},
		{"branch unknown", repos.ErrLensBranchUnknown, http.StatusUnprocessableEntity, "Lens pins an unknown branch"},
		{"write repo subscribed", repos.ErrLensWriteSubscribed, http.StatusUnprocessableEntity, "Lens write repo is a subscription"},
		{"write repo fleet", repos.ErrLensWriteFleet, http.StatusUnprocessableEntity, "Lens write repo is the fleet repository"},
		{"write empty", control.ErrLensWriteEmpty, http.StatusBadRequest, "Lens write repo required"},
		{"description too long", control.ErrLensDescriptionTooLong, http.StatusUnprocessableEntity, "Lens description too long"},
		{"lens not found", control.ErrLensNotFound, http.StatusNotFound, "Lens not found"},
		{"unmapped", errors.New("boom"), http.StatusInternalServerError, "Create lens failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title := lensCreateErrStatus(tc.err)
			if status != tc.wantStatus || title != tc.wantTitle {
				t.Errorf("got (%d, %q), want (%d, %q)", status, title, tc.wantStatus, tc.wantTitle)
			}
		})
	}
}

// TestLensPatchErrStatus pins the PATCH mapping (m13): the validation arms are
// byte-identical to lensCreateErrStatus, but the scrubbed-500 default arm carries
// an operation-appropriate title so a PATCH failure never reads "Create lens
// failed".
func TestLensPatchErrStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		// 4xx/422 arms mirror create exactly (byte-identity is a hard constraint).
		{"invalid name", repos.ErrInvalidLensName, http.StatusBadRequest, "Invalid lens name"},
		{"name conflicts repo", repos.ErrLensNameConflictsRepo, http.StatusConflict, "Lens name conflicts with a repo"},
		{"lens exists", control.ErrLensExists, http.StatusConflict, "Lens already exists"},
		{"create in flight", repos.ErrCreateInFlight, http.StatusConflict, "Create in flight"},
		{"replica in lens", repos.ErrReplicaInLens, http.StatusConflict, "Replica mounts not allowed"},
		{"repo not found", repos.ErrRepoNotFound, http.StatusUnprocessableEntity, "Lens references an unknown repo"},
		{"branch unknown", repos.ErrLensBranchUnknown, http.StatusUnprocessableEntity, "Lens pins an unknown branch"},
		{"write repo subscribed", repos.ErrLensWriteSubscribed, http.StatusUnprocessableEntity, "Lens write repo is a subscription"},
		{"write repo fleet", repos.ErrLensWriteFleet, http.StatusUnprocessableEntity, "Lens write repo is the fleet repository"},
		{"write empty", control.ErrLensWriteEmpty, http.StatusBadRequest, "Lens write repo required"},
		{"description too long", control.ErrLensDescriptionTooLong, http.StatusUnprocessableEntity, "Lens description too long"},
		{"lens not found", control.ErrLensNotFound, http.StatusNotFound, "Lens not found"},
		// The only divergence: the unmapped 500 default arm names the PATCH op.
		{"unmapped", errors.New("boom"), http.StatusInternalServerError, "Update lens failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title := lensPatchErrStatus(tc.err)
			if status != tc.wantStatus || title != tc.wantTitle {
				t.Errorf("got (%d, %q), want (%d, %q)", status, title, tc.wantStatus, tc.wantTitle)
			}
		})
	}
}
