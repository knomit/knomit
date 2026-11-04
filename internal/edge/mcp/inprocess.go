package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/clientsessions"
)

// The in-process MCP session: how the chat reaches knomit's tools. It speaks to
// the same *server.MCPServer every external agent does, so permissions,
// binding gates and validation are the ordinary ones; chat never calls a
// handler or a corpus API directly.
//
// mcp-go's own in-process client (client.NewInProcessClient, v0.45.0,
// client/transport/inprocess.go) cannot be used: its transport registers a
// server-side session only when sampling, elicitation or roots handlers are
// configured, and without a registered session AfterInitialize's
// recordClientInfo finds no session id and records nothing. So this file has
// its own transport.Interface that registers a server.InProcessSession under
// a caller-supplied id, and records every request in clientsessions through
// the same RecordRequest the HTTP dispatch uses.

// InProcessOptions describes who the chat acts as and how it is addressed.
type InProcessOptions struct {
	// Principal is the signed-in user; every call carries it. Ceiling,
	// RepoReach and LensReach are copied onto each request only when set,
	// exactly as the web layer's mcpHTTPContext copies what the auth edge
	// attached: nil means the edge attached none.
	Principal auth.Principal
	Ceiling   *auth.Set
	RepoReach func(repo string) bool
	LensReach func(lens string) bool

	// SessionID is the canonical chat id "chat-<32 hex>" (plan P24). It is the
	// MCP session id, the client_sessions row id and the session log's stem.
	SessionID string
	// Endpoint is "/api/v1/chats/<SessionID>". With the principal it OWNS what
	// the chat's calls create for later calls to resume (query/explain/
	// materialize cursors, tasks), so a cursor minted by one chat reads as
	// expired in any other.
	Endpoint string
	// ClientName and ClientVersion are the initialize clientInfo: "knomit-chat"
	// and "<provider>/<model>". They are what marks the Sessions row as a chat.
	ClientName, ClientVersion string
}

// InProcessSession is one chat's MCP connection.
type InProcessSession struct {
	t      *inProcessTransport
	client *client.Client
	store  *clientsessions.Store
	id     string
	closed sync.Once
}

// OpenInProcessSession registers a session on srv, initializes it as a chat
// client and loads the prefix: the initialize instructions (hook 3) and the
// tools the principal may see (hook 1).
func OpenInProcessSession(ctx context.Context, srv *server.MCPServer, mgr *repos.Manager, o InProcessOptions) (*InProcessSession, string, []llm.ToolDef, error) {
	switch {
	case srv == nil:
		return nil, "", nil, errors.New("in-process mcp session: no server")
	case o.SessionID == "":
		return nil, "", nil, errors.New("in-process mcp session: no session id")
	case o.Endpoint == "":
		return nil, "", nil, errors.New("in-process mcp session: no endpoint")
	case o.Principal.IsZero():
		// requestContext attaches the principal to every call; a zero one
		// would let the permission gate see an empty identity.
		return nil, "", nil, errors.New("in-process mcp session: no principal")
	}
	t := &inProcessTransport{srv: srv, mgr: mgr, o: o}
	s := &InProcessSession{t: t, client: client.NewClient(t), id: o.SessionID}
	if mgr != nil {
		s.store = mgr.ClientSessions()
	}
	instructions, tools, err := LoadPrefix(ctx, s.client, mcpgo.Implementation{Name: o.ClientName, Version: o.ClientVersion})
	if err != nil {
		_ = s.Close(context.WithoutCancel(ctx))
		return nil, "", nil, err
	}
	return s, instructions, tools, nil
}

// ID is the canonical chat id.
func (s *InProcessSession) ID() string { return s.id }

// CallTool runs a tool through the MCP server and flattens its content to
// text. A protocol-level failure (unknown tool, transport) is returned as an
// error; a tool that ran and refused is a ToolOutcome with IsError. It
// implements llm.ToolHost.
func (s *InProcessSession) CallTool(ctx context.Context, name string, args json.RawMessage) (llm.ToolOutcome, error) {
	var req mcpgo.CallToolRequest
	req.Params.Name = name
	if len(args) > 0 {
		var m map[string]any
		if err := json.Unmarshal(args, &m); err != nil {
			return llm.ToolOutcome{}, fmt.Errorf("arguments for %s are not a JSON object: %w", name, err)
		}
		req.Params.Arguments = m
	}
	res, err := s.client.CallTool(ctx, req)
	if err != nil {
		return llm.ToolOutcome{}, err
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			parts = append(parts, tc.Text)
			continue
		}
		raw, merr := json.Marshal(c)
		if merr != nil {
			return llm.ToolOutcome{}, fmt.Errorf("tool %s: unreadable content: %w", name, merr)
		}
		parts = append(parts, string(raw))
	}
	return llm.ToolOutcome{Content: strings.Join(parts, "\n"), IsError: res.IsError}, nil
}

// TargetPin resolves the call's `binding` handle to the repo or lens it
// names ("repo:<uid>" or "lens:<uid>"), which is what a per-target approval
// grant is keyed on. ok is false when the call carries no handle or the
// handle does not resolve; such a call is not prompted, and the binding gate
// refuses it before any handler code runs.
func (s *InProcessSession) TargetPin(ctx context.Context, args map[string]any) (string, bool) {
	handle, _ := args["binding"].(string)
	if handle == "" || s.store == nil {
		return "", false
	}
	pin, _, ok, err := s.store.BindingHandle(ctx, handle, time.Now())
	if err != nil || !ok {
		return "", false
	}
	return pin, true
}

// Close ends the session row and unregisters the server-side session. Safe to
// call more than once. A call still in flight finishes, but it records
// nothing once Close has begun: clientsessions.Store.Touch clears ended_at, so
// a late record would bring the ended row back to life.
func (s *InProcessSession) Close(ctx context.Context) error {
	var err error
	s.closed.Do(func() {
		s.t.stopRecording()
		if s.store != nil {
			err = s.store.End(context.WithoutCancel(ctx), s.id, time.Now())
		}
		if cerr := s.t.Close(); err == nil {
			err = cerr
		}
	})
	return err
}

// inProcessTransport is a transport.Interface onto an *server.MCPServer.
type inProcessTransport struct {
	srv *server.MCPServer
	mgr *repos.Manager
	o   InProcessOptions

	mu      sync.Mutex
	session *server.InProcessSession

	// recMu orders record against stopRecording: record holds the read lock
	// across the store write, stopRecording takes the write lock, so once it
	// returns no record is running and none will start. Close calls it before
	// store.End.
	recMu     sync.RWMutex
	noRecords bool
}

var _ transport.Interface = (*inProcessTransport)(nil)

func (t *inProcessTransport) Start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session != nil {
		return nil
	}
	sess := server.NewInProcessSession(t.o.SessionID, nil)
	if err := t.srv.RegisterSession(ctx, sess); err != nil {
		return fmt.Errorf("in-process mcp session: register %s: %w", t.o.SessionID, err)
	}
	t.session = sess
	return nil
}

// requestContext builds the context one request is served under. It mirrors
// the web layer's mcpHTTPContext field by field, plus what the unscoped mount's
// SessionBindingMiddleware adds:
//
//	clientsessions.WithHTTPInfo   empty ip and User-Agent (there is no socket)
//	auth principal, ceiling, repo reach, lens reach   copied when set
//	WithEndpoint                  the chat's own endpoint path (cursor owner)
//	repos.WithSessionScoped       the unscoped endpoint kind (needs a handle)
//	repos.WithPinRecorder         a fresh recorder, read back for RecordRequest
//
// The kernel-verified peer pid (clientsessions.WithVerifiedPID) is not copied:
// a chat has no socket peer.
func (t *inProcessTransport) requestContext(ctx context.Context) (context.Context, *repos.PinRecorder) {
	ctx = clientsessions.WithHTTPInfo(ctx, "", "")
	ctx = auth.WithPrincipal(ctx, t.o.Principal)
	if t.o.Ceiling != nil {
		ctx = auth.WithCeiling(ctx, *t.o.Ceiling)
	}
	if t.o.RepoReach != nil {
		ctx = auth.WithRepoReach(ctx, t.o.RepoReach)
	}
	if t.o.LensReach != nil {
		ctx = auth.WithLensReach(ctx, t.o.LensReach)
	}
	ctx = WithEndpoint(ctx, t.o.Endpoint)
	ctx = repos.WithSessionScoped(ctx)
	rec := &repos.PinRecorder{}
	ctx = repos.WithPinRecorder(ctx, rec)
	t.mu.Lock()
	sess := t.session
	t.mu.Unlock()
	return t.srv.WithContext(ctx, sess), rec
}

func (t *inProcessTransport) SendRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if t.sessionOrNil() == nil {
		return nil, errors.New("in-process mcp session: not started")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	rctx, rec := t.requestContext(ctx)
	msg := t.srv.HandleMessage(rctx, append(raw, '\n'))
	t.record(rctx, rec)

	out, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal response: %w", err)
	}
	var resp transport.JSONRPCResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &resp, nil
}

func (t *inProcessTransport) SendNotification(ctx context.Context, n mcpgo.JSONRPCNotification) error {
	if t.sessionOrNil() == nil {
		return errors.New("in-process mcp session: not started")
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	rctx, _ := t.requestContext(ctx)
	t.srv.HandleMessage(rctx, append(raw, '\n'))
	return nil
}

func (t *inProcessTransport) sessionOrNil() *server.InProcessSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.session
}

// record notes one served request through the single recorder the HTTP
// dispatch also uses (clientsessions.Store.RecordRequest). Errors are logged
// and dropped, like the HTTP side.
func (t *inProcessTransport) record(ctx context.Context, rec *repos.PinRecorder) {
	if t.mgr == nil {
		return
	}
	t.recMu.RLock()
	defer t.recMu.RUnlock()
	if t.noRecords {
		return
	}
	store := t.mgr.ClientSessions()
	if store == nil {
		return
	}
	resolved := rec.Resolved()
	obs := clientsessions.Observation{
		SessionID: t.o.SessionID,
		Binding:   resolved.Pin,
		Principal: t.o.Principal.String(),
		Now:       time.Now(),
	}
	if err := store.RecordRequest(context.WithoutCancel(ctx), obs, resolved.Handle, resolved.Branch); err != nil {
		log.Warn().Err(err).Str("mcp_session", t.o.SessionID).Msg("client sessions: record request failed")
	}
}

// stopRecording makes every later record a no-op and waits for one in progress.
func (t *inProcessTransport) stopRecording() {
	t.recMu.Lock()
	t.noRecords = true
	t.recMu.Unlock()
}

// SetNotificationHandler is deliberately a no-op: server-to-client
// notifications (task status broadcasts and the like) are discarded. The
// server's non-blocking send to a full session channel drops them too, as
// mcp-go's own transport does; a chat has nothing that consumes them.
func (t *inProcessTransport) SetNotificationHandler(func(mcpgo.JSONRPCNotification)) {}

func (t *inProcessTransport) Close() error {
	t.mu.Lock()
	sess := t.session
	t.session = nil
	t.mu.Unlock()
	if sess != nil {
		t.srv.UnregisterSession(context.Background(), t.o.SessionID)
	}
	return nil
}

func (t *inProcessTransport) GetSessionId() string { return t.o.SessionID }
