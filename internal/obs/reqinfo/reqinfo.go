// Package reqinfo carries a small, mutable annotation alongside an in-flight
// request so a subsystem deep in the call tree can tell the outermost layer
// what the request turned out to be doing.
//
// Context values normally flow one way — a handler cannot hand anything back to
// the middleware that wrapped it. The middleware therefore stores a POINTER
// before dispatching; whoever fills it in is writing into memory the middleware
// still holds a reference to. That is what lets the HTTP slow-request log name
// the MCP tool that was slow, when the tool name is only known once the MCP
// server has parsed the JSON-RPC body.
//
// It is a leaf package (stdlib only) so both internal/web and internal/mcp can
// import it — internal/web imports internal/mcp, so nothing shared may live in
// either one.
package reqinfo

import (
	"context"
	"sync"
)

// Info is the per-request annotation. The zero value is not usable; get one
// from NewContext. A nil *Info is a valid no-op sink, so callers annotate
// unconditionally without checking whether a carrier is present.
type Info struct {
	mu   sync.Mutex
	tool string
}

type ctxKey struct{}

// NewContext returns ctx with a fresh Info attached, plus that Info for the
// caller to read once the request is done.
func NewContext(ctx context.Context) (context.Context, *Info) {
	info := &Info{}
	return context.WithValue(ctx, ctxKey{}, info), info
}

// FromContext returns the Info carried by ctx, or nil when there is none —
// stdio MCP sessions and most tests carry no Info. The nil is safe to use.
func FromContext(ctx context.Context) *Info {
	info, _ := ctx.Value(ctxKey{}).(*Info)
	return info
}

// SetTool records the MCP tool this request dispatched to.
func (i *Info) SetTool(name string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.tool = name
}

// Tool returns the recorded MCP tool name, or "" if none was recorded.
func (i *Info) Tool() string {
	if i == nil {
		return ""
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.tool
}
