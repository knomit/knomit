package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStore_InitFromRemote_HonoursCtx: a create's clone runs under the
// lifecycle machine's Populate life, and an Unmount (a cancelled create, a
// shutdown) cancels that life's ctx. InitFromRemote threads the ctx into its
// fetch, so a cancel aborts a transfer in flight instead of waiting it out.
//
// The remote accepts the request and never answers; the cancel lands once the
// request has provably arrived, so there is no timing to tune. No network
// timeout is set, so only the ctx can end the fetch.
func TestStore_InitFromRemote_HonoursCtx(t *testing.T) {
	arrived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, ierr := svc.InitFromRemote(ctx, srv.URL+"/kb.git", nil, "", "agent/x", nil, nil)
		done <- ierr
	}()

	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the clone never reached the remote")
	}
	cancel()
	select {
	case ierr := <-done:
		require.Error(t, ierr)
		require.ErrorContains(t, ierr, "context canceled", "the fetch must end on the caller's ctx")
	case <-time.After(30 * time.Second):
		t.Fatal("InitFromRemote ignored its ctx: the cancelled clone kept waiting on the remote")
	}
}
