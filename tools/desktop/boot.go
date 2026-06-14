//go:build desktop

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"knomit/tools/desktop/internal/lockfile"
	"knomit/tools/desktop/internal/netutil"
)

// server bundles a running http.Server with its discovery lockfile path and a
// cancel for the per-request base context.
type server struct {
	http     *http.Server
	cancel   context.CancelFunc
	lockPath string
}

// shutdown stops the server and removes the discovery lockfile. It cancels the
// request base context FIRST so long-lived handlers (the SSE /events stream,
// which selects on r.Context().Done()) return immediately — otherwise
// http.Server.Shutdown blocks for the full timeout waiting for that connection
// to drain, making Quit feel slow.
func (s *server) shutdown() {
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
	_ = lockfile.Remove(s.lockPath)
}

// bootServer picks a looknomitck port (netutil.PickPort prefers 19278, falling
// back to an ephemeral port only if 19278 is taken), serves handler on it,
// writes the discovery lockfile, and returns the running server and chosen
// port. External MCP clients discover the port via the lockfile.
//
// parent is the application context; it is propagated into every request via
// BaseContext so a single cancel (on shutdown, or when parent is cancelled)
// unblocks streaming handlers promptly.
func bootServer(parent context.Context, handler http.Handler, lockPath, version string) (*server, int, error) {
	port, err := netutil.PickPort()
	if err != nil {
		return nil, 0, fmt.Errorf("pick port: %w", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, 0, fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	srvCtx, cancel := context.WithCancel(parent)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // 0 = no limit, required for SSE long-poll streams
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return srvCtx },
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "desktop serve error: %v\n", err)
		}
	}()
	if err := lockfile.Write(lockPath, lockfile.Info{PID: os.Getpid(), Port: port, Version: version}); err != nil {
		cancel()
		_ = srv.Close()
		return nil, 0, fmt.Errorf("write lockfile: %w", err)
	}
	return &server{http: srv, cancel: cancel, lockPath: lockPath}, port, nil
}
