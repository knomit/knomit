package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestHandler_AdvertisesOKFRefAfterTrigger regresses the wiring between the
// smart-HTTP advertise path and lazy OKF generation: hitting /info/refs must
// trigger EnsureOKF for the branches we publish (main + the agent branch)
// before the ref advertisement is built, so a fresh clone already sees
// okf/<branch> refs without any separate generation step.
func TestHandler_AdvertisesOKFRefAfterTrigger(t *testing.T) {
	ctx := context.Background()

	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/scope/d9d6557d.md",
		testFactBody("Scope", 0.9, nil), "seed", "learn")
	require.NoError(t, err)

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	defer resp.Body.Close()

	// After the advertise trigger fired, the okf/main ref exists in the store.
	_, err = svc.rh.gits.Reference(plumbing.NewBranchReferenceName("okf/main"))
	require.NoError(t, err, "okf/main not generated on advertise")
}
