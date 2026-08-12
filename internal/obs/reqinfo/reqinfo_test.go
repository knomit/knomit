package reqinfo

import (
	"context"
	"sync"
	"testing"
)

func TestNewContextRoundTrips(t *testing.T) {
	ctx, info := NewContext(context.Background())
	if info == nil {
		t.Fatal("NewContext returned a nil Info")
	}
	if got := FromContext(ctx); got != info {
		t.Errorf("FromContext = %p, want the Info NewContext returned (%p)", got, info)
	}
}

func TestFromContextAbsentIsNil(t *testing.T) {
	if got := FromContext(context.Background()); got != nil {
		t.Errorf("FromContext on a bare context = %p, want nil", got)
	}
}

// A nil *Info must behave like a working sink: subsystems annotate
// unconditionally, and the stdio MCP transport (or any test) carries no Info at
// all. Panicking there would turn a logging nicety into an outage.
func TestNilInfoIsSafe(t *testing.T) {
	var info *Info
	info.SetTool("knomit_query") // must not panic
	if got := info.Tool(); got != "" {
		t.Errorf("(*Info)(nil).Tool() = %q, want \"\"", got)
	}
}

func TestSetToolIsReadBack(t *testing.T) {
	_, info := NewContext(context.Background())
	info.SetTool("knomit_learn")
	if got := info.Tool(); got != "knomit_learn" {
		t.Errorf("Tool() = %q, want %q", got, "knomit_learn")
	}
}

// The writer (an MCP tool hook) and the reader (the HTTP middleware's deferred
// log) are not guaranteed to be the same goroutine, so the field is mutex-held.
func TestConcurrentSetAndReadIsRaceFree(t *testing.T) {
	_, info := NewContext(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); info.SetTool("knomit_query") }()
		go func() { defer wg.Done(); _ = info.Tool() }()
	}
	wg.Wait()
	if got := info.Tool(); got != "knomit_query" {
		t.Errorf("Tool() = %q, want %q", got, "knomit_query")
	}
}
