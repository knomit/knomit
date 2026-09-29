//go:build windows

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"knomit/internal/auth"
)

// knomit#344: closing the local listener must not hang after a client has
// connected and hung up. go-winio v0.6.2 could lose its one close signal to a
// pending ConnectNamedPipe that returned ERROR_OPERATION_ABORTED, and Close then
// waited forever. That is every server's shutdown, and it surfaced as
// TestOAuthCLI_ForeignListenerIsSurfaced hitting the 10m timeout in cleanup.
//
// A refused dial is the trigger: DialLocal connects, reads the owner, refuses
// it and hangs up. On v0.6.2 this path hung about once in 35 rounds
// (measured), so 300 rounds catch a regression all but certainly in a few
// seconds. Close is bounded so a regression fails here, not as a timeout.
func TestLocalListener_CloseAfterRefusedDialDoesNotHang(t *testing.T) {
	const rounds = 300
	base := oauthLocalListenerPath(t)
	t.Cleanup(auth.RefuseEveryPipeOwnerForTest())
	for i := 0; i < rounds; i++ {
		path := fmt.Sprintf("%s-%d", base, i)
		ln, cleanup, err := auth.ListenLocal(path)
		if err != nil {
			t.Fatalf("round %d: ListenLocal: %v", i, err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
		go func() { _ = srv.Serve(ln) }()

		err = oauthPending(context.Background(), localAPIClient(path), &bytes.Buffer{})
		if !errors.Is(err, auth.ErrForeignListener) {
			cleanup()
			t.Fatalf("round %d: want auth.ErrForeignListener, got %v", i, err)
		}

		done := make(chan struct{})
		go func() { srv.Close(); close(done) }()
		select {
		case <-done:
			cleanup()
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: closing the listener hung after a refused dial (knomit#344); "+
				"is go-winio older than microsoft/go-winio#388?", i)
		}
	}
}
