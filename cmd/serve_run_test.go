package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"path/filepath"
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
	go func() { done <- serveUntil(context.Background(), errCh, func() {}, time.Second, srv) }()
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
	if err := serveUntil(ctx, errCh, func() {}, 5*time.Second, primary, nil, other); err != nil {
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

// A listener failure must end a long-lived request (an SSE stream selects on
// r.Context().Done()) the way SIGINT does: serveUntil cancels the servers'
// BaseContext before Shutdown. Without that the stream holds Shutdown for the
// whole grace, and the returned error carries context.DeadlineExceeded.
func TestServeUntil_ListenerFailureUnblocksStreams(t *testing.T) {
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	entered := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
		}),
		BaseContext: func(net.Listener) context.Context { return serveCtx },
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	serveInto(errCh, "listen", srv, ln)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream request never reached the handler")
	}

	errCh <- errors.New("local authenticated listener: boom")
	const grace = 3 * time.Second
	start := time.Now()
	err = serveUntil(context.Background(), errCh, cancelServe, grace, srv)
	if elapsed := time.Since(start); elapsed >= grace {
		t.Fatalf("serveUntil took %v: the open stream held Shutdown for the whole grace", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want the listener failure returned, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown timed out waiting for the stream: %v", err)
	}
}

// Nothing in cmd/ may exit the process on its own: an exit skips RunE's
// defers, and on the serve path the deferred release of the crash marker is
// one of them, so the next boot reports a crash that did not happen (#261).
// Commands return errors and main.go exits.
//
// Every non-test .go file in cmd/ is scanned, so a new serve_*.go file is
// covered without editing this list, and a Fatal* call is caught on ANY
// receiver (log, a logger variable, zerolog's event chain). The one allowed
// exit is verify's: it exits with its documented codes after runVerify's
// defers have run, and verify sets no crash marker. Allowed by exact count, so
// a third exit there still fails.
//
// This guard is cmd-scoped. internal/web/server.go Server.grants() still calls
// log.Fatal on an invalid [auth].loopback_default; serve cannot reach it only
// because app.New's seedOwnPrincipal parses the same value first and returns
// the error. That is deliberately out of scope here.
func TestCmd_NoProcessExit(t *testing.T) {
	allowed := map[string]int{"verify.go": 2} // os.Exit(code), os.Exit(exitFailed)
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var exits []string
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch name := sel.Sel.Name; {
			case name == "Fatal" || name == "Fatalf" || name == "Fatalln":
			case name == "Exit":
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
					return true
				}
			default:
				return true
			}
			exits = append(exits, fset.Position(sel.Pos()).String()+" "+sel.Sel.Name)
			return true
		})
		if len(exits) != allowed[name] {
			t.Errorf("%s: %d process exit(s), %d allowed; return the error instead:\n  %s",
				name, len(exits), allowed[name], strings.Join(exits, "\n  "))
		}
	}
	if scanned < 10 {
		t.Fatalf("scanned only %d files; the glob is not looking at cmd/", scanned)
	}
}
