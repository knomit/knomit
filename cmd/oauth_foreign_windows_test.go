//go:build windows

package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"knomit/internal/auth"
)

// knomit#265: `knomit oauth` shows a consent screen and sends approvals over
// the local listener, so a process squatting its name under another account
// must be refused, and the refusal must say so — not be dressed up as "no
// server, start knomit serve". The squatter is a real listener of this test's
// own with auth told to refuse every owner; a pipe owned by another account
// cannot be created without privileges the suite does not have.
func TestOAuthCLI_ForeignListenerIsSurfaced(t *testing.T) {
	path := oauthLocalListenerPath(t)
	ln, cleanup, err := auth.ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal: %v", err)
	}
	t.Cleanup(cleanup)
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"pending":[]}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	t.Cleanup(auth.RefuseEveryPipeOwnerForTest())

	err = oauthPending(context.Background(), localAPIClient(path), &bytes.Buffer{})
	if !errors.Is(err, auth.ErrForeignListener) {
		t.Fatalf("want auth.ErrForeignListener in the chain, got %v", err)
	}
	if strings.Contains(err.Error(), "knomit serve") {
		t.Fatalf("a foreign listener must not be reported as a missing server: %v", err)
	}
	// This CLI honours no named server, so it must not be advised to name one.
	if strings.Contains(err.Error(), "KNOMIT_SERVER") || strings.Contains(err.Error(), "server argument") {
		t.Fatalf("`knomit oauth` has no URL escape hatch, yet its error offers one: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the refused listener received %d request(s)", n)
	}
}
