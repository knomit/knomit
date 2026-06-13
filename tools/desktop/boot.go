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

// server bundles a running http.Server with its discovery lockfile path.
type server struct {
	http     *http.Server
	lockPath string
}

// shutdown gracefully stops the server and removes the discovery lockfile.
func (s *server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
	_ = lockfile.Remove(s.lockPath)
}

// bootServer picks a looknomitck port (netutil.PickPort prefers 19278, falling
// back to an ephemeral port only if 19278 is taken), serves handler on it,
// writes the discovery lockfile, and returns the running server and chosen
// port. External MCP clients discover the port via the lockfile.
func bootServer(handler http.Handler, lockPath, version string) (*server, int, error) {
	port, err := netutil.PickPort()
	if err != nil {
		return nil, 0, fmt.Errorf("pick port: %w", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, 0, fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // 0 = no limit, required for SSE long-poll streams
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "desktop serve error: %v\n", err)
		}
	}()
	if err := lockfile.Write(lockPath, lockfile.Info{PID: os.Getpid(), Port: port, Version: version}); err != nil {
		_ = srv.Close()
		return nil, 0, fmt.Errorf("write lockfile: %w", err)
	}
	return &server{http: srv, lockPath: lockPath}, port, nil
}
