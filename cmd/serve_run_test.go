package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListenTCP_BindFailureIsAnError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	ln, err := listenTCP(busy.Addr().String())
	if err == nil {
		ln.Close()
		t.Fatal("binding a taken address succeeded")
	}
	if !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("error must name the address: %v", err)
	}
}

// A listener that fails while serving ends the wait with its error rather
// than ending the process: serveUntil returns it, so RunE's defers (the crash
// marker's release among them) still run.
func TestServeUntil_ListenerFailureIsReturned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	errCh := make(chan error, 1)
	serveInto(errCh, "local authenticated listener", srv, ln)
	// Closing the listener out from under Serve is a failure Serve reports
	// as an error other than http.ErrServerClosed.
	ln.Close()

	done := make(chan error, 1)
	go func() { done <- serveUntil(context.Background(), errCh, time.Second, srv) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serveUntil returned nil after a listener failed")
		}
		if !errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "local authenticated listener") {
			t.Fatalf("want the listener's error, named; got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveUntil did not return after a listener failed")
	}
}

// A clean stop reports nothing: every Serve returns http.ErrServerClosed once
// Shutdown runs, and none of those may surface as a failure.
func TestServeUntil_CleanShutdownReportsNothing(t *testing.T) {
	primary := &http.Server{Handler: http.NotFoundHandler()}
	other := &http.Server{Handler: http.NotFoundHandler()}
	errCh := make(chan error, 3)
	for _, s := range []*http.Server{primary, primary, other} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		serveInto(errCh, "listener", s, ln)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A nil entry stands for a listener that is not configured.
	if err := serveUntil(ctx, errCh, 5*time.Second, primary, nil, other); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	// The Serve goroutines return just after Shutdown closes their listeners;
	// a spurious send would land within this window.
	select {
	case err := <-errCh:
		t.Fatalf("a clean shutdown reported a listener failure: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// Nothing reachable from serve's RunE may exit the process: an exit skips the
// deferred release of the crash marker, and the next boot then reports a crash
// that did not happen (#261). Errors are returned, and main.go exits.
func TestServe_NoProcessExitOnTheServePath(t *testing.T) {
	files := []string{"serve.go", "serve_tls.go", "serve_oauth.go", "listen.go", "signals_unix.go", "signals_other.go"}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if (pkg.Name == "log" && strings.HasPrefix(sel.Sel.Name, "Fatal")) ||
				(pkg.Name == "os" && sel.Sel.Name == "Exit") {
				t.Errorf("%s: %s.%s exits the process; return the error instead", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
}
