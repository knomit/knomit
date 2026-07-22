package web

import (
	"errors"
	"net/http"
	"testing"

	"knomit/internal/repos"
)

// TestLensResolveStatus pins the mapping from a resolution failure kind onto
// its HTTP status and problem title. Resolution itself is tested in
// internal/repos (TestResolveLensBinding_*); this is the rendering half.
func TestLensResolveStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		{
			name:       "registry not started",
			err:        &repos.LensResolveError{Kind: repos.LensRegistryUnavailable, Err: errors.New("x")},
			wantStatus: http.StatusServiceUnavailable,
			wantTitle:  "Lens registry unavailable",
		},
		{
			name:       "registry lookup failed",
			err:        &repos.LensResolveError{Kind: repos.LensRegistryError, Err: errors.New("x")},
			wantStatus: http.StatusInternalServerError,
			wantTitle:  "Lens registry error",
		},
		{
			name:       "unknown lens",
			err:        &repos.LensResolveError{Kind: repos.LensNotFound, Err: errors.New("x")},
			wantStatus: http.StatusNotFound,
			wantTitle:  "Lens not found",
		},
		{
			name:       "member repo unavailable",
			err:        &repos.LensResolveError{Kind: repos.LensUnavailable, Err: errors.New("x")},
			wantStatus: http.StatusServiceUnavailable,
			wantTitle:  "Lens unavailable",
		},
		{
			// An unclassified error must not be mistaken for a client fault.
			name:       "unclassified",
			err:        errors.New("boom"),
			wantStatus: http.StatusInternalServerError,
			wantTitle:  "Lens registry error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title := lensResolveStatus(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status: got %d, want %d", status, tc.wantStatus)
			}
			if title != tc.wantTitle {
				t.Errorf("title: got %q, want %q", title, tc.wantTitle)
			}
		})
	}
}
