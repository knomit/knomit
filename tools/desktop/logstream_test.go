//go:build desktop

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The stream is wired to Wails' Emit in production. Here the emitter is a
// closure, which is the whole reason startLogStream takes one rather than
// reaching for the App itself.
func TestStartLogStreamEmitsFileLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startLogStream(ctx, path, func(batch []string) {
		mu.Lock()
		got = append(got, batch...)
		mu.Unlock()
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no lines emitted")
}

// The tailer runs for as long as the app does, so the only thing that stops it
// is the context. A leak here is invisible in production (one goroutine polling
// a file forever) and would only ever show up as a test binary that will not
// exit, so assert the shutdown path directly.
func TestStartLogStreamStopsWhenContextCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	emitted := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		startLogStreamBlocking(ctx, path, func([]string) {
			select {
			case emitted <- struct{}{}:
			default:
			}
		})
		close(done)
	}()

	select {
	case <-emitted:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("no lines emitted")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream still running after the context was cancelled")
	}
}

// A missing file must not be fatal: the Logs window can be opened before
// anything has been written, and logPathFor returns "" when the logs directory
// cannot be resolved at all.
func TestStartLogStreamToleratesMissingFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startLogStream(ctx, filepath.Join(t.TempDir(), "absent.log"), func([]string) {
		t.Error("emitted lines for a file that does not exist")
	})
	time.Sleep(50 * time.Millisecond)
}

// recordingTarget stands in for the Logs window.
type recordingTarget struct {
	events []*application.CustomEvent
}

func (r *recordingTarget) DispatchWailsEvent(event *application.CustomEvent) {
	r.events = append(r.events, event)
}

// The regression this guards is a memory leak with no symptom: app.Event.Emit
// fans out to every window, and the main knowledge window (served from web/,
// which never loads the Wails runtime) queues every batch in an unbounded
// pendingJS slice that is never flushed. Nothing observable goes wrong — the
// Logs window works either way — the process just grows for as long as it logs.
//
// Typing the emitter to a single target is what makes that unrepeatable, so
// this asserts the payload the ONE window receives. The compile-time
// `var _ logEventTarget = (*application.WebviewWindow)(nil)` in logstream.go is
// the other half: it keeps the seam honest about what it stands for.
func TestLogEmitterAddressesASingleWindow(t *testing.T) {
	target := &recordingTarget{}
	batch := []string{"1:12PM INF tray up", "1:12PM WRN synthesis disabled"}

	newLogEmitter(target)(batch)

	if len(target.events) != 1 {
		t.Fatalf("dispatched %d events, want exactly 1", len(target.events))
	}
	got := target.events[0]
	if got.Name != logEventName {
		t.Errorf("event name = %q, want %q", got.Name, logEventName)
	}
	// []string, not a wrapper: the frontend reads event.data as the batch
	// itself (see logStore.ts), and single-argument Emit had the same shape.
	lines, ok := got.Data.([]string)
	if !ok {
		t.Fatalf("event data is %T, want []string", got.Data)
	}
	if !slices.Equal(lines, batch) {
		t.Errorf("event data = %q, want %q", lines, batch)
	}
}

// tsEventsModule is the single place the frontend names the event, relative to
// this test's working directory (tools/desktop).
const tsEventsModule = "ui/src/events.ts"

// Go emits on a name the webview subscribes to by the same name, and neither
// side can see the other's spelling. Nothing fails loudly when they drift — the
// window just sits there empty forever — so pin the two halves together here.
// This is why the TypeScript side keeps the name in a module of its own rather
// than inline in the component: a bare string in a .tsx file is one refactor
// away from being two bare strings.
func TestLogEventNameMatchesFrontend(t *testing.T) {
	src, err := os.ReadFile(tsEventsModule)
	if err != nil {
		t.Fatalf("read %s: %v", tsEventsModule, err)
	}
	want := "export const LOG_EVENT = '" + logEventName + "'"
	if !strings.Contains(string(src), want) {
		t.Errorf("%s does not declare %q; Go emits on %q and the Logs window would never see it",
			tsEventsModule, want, logEventName)
	}
}
