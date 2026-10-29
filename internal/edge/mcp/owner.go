package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"knomit/internal/foundation/auth"
)

type endpointKey struct{}

// WithEndpoint records the MCP endpoint a request arrived on: its URL path
// (/api/v1/mcp, /api/v1/repos/<r>/branches/<b>/mcp, /api/v1/lenses/<l>/mcp, …).
// The web layer's HTTPContextFunc sets it on every MCP request. A trailing "/"
// is not part of the endpoint.
func WithEndpoint(ctx context.Context, path string) context.Context {
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	return context.WithValue(ctx, endpointKey{}, path)
}

// callerOwner is WHO owns what a call creates for a later call to pick up —
// a tool-session cursor (tool_sessions / tool_queue) or an MCP task: the
// endpoint the call arrived on plus the caller's principal.
//
// The principal is the IDENTITY, not the transport: the local OS user a
// socket or pipe peer resolves to (uid:<n>, sid:<SID>), or an OAuth token's
// subject (host:<subject>@token, the subject the approver named or the
// identity provider's account id — never the client app's id). Two bridges of
// one user on one endpoint are therefore one owner; two users, or one user on
// two endpoints, are two.
//
// A context with neither (an in-process caller or a test) owns as "", and
// only another such caller matches it: production MCP traffic always carries
// an endpoint.
func callerOwner(ctx context.Context) string {
	endpoint, _ := ctx.Value(endpointKey{}).(string)
	p, _ := auth.FromContext(ctx)
	if endpoint == "" && p.IsZero() {
		return ""
	}
	return endpoint + " " + p.String()
}

// errTaskNotFound is mcp-go's own answer for an unknown task id. A task owned
// by another endpoint or principal answers exactly this, so a caller cannot
// tell "exists, not yours" from "never existed".
const errTaskNotFound = "task not found"

// errUnreadableRequest refuses a request the task-ownership hook cannot read.
// mcp-go has already parsed the envelope before the hook runs, so this is
// unreachable through the transport today; it exists so the hook fails closed.
const errUnreadableRequest = "invalid request"

// taskOwnerRetention is how long a finished task's owner stays recorded. A
// task is refused to everyone once its owner is forgotten, the owner
// included, so this bounds the record without ever widening access: mcp-go
// keeps a task with no client TTL for the life of the process, and an owner
// that has not fetched its result an hour after it finished reads it as gone.
const taskOwnerRetention = time.Hour

// taskOwners is the MCP task ownership table (audit G2). mcp-go keys its one
// server-wide task map by MCP session id alone, and knomit's session ids are
// unverified (any well-formed id is accepted), so a caller presenting another
// caller's session id could read or cancel that caller's tasks from any
// endpoint. tasks/* are not tool calls: neither the binding gate nor the
// permission gate runs on them. This table closes that by owner instead.
type taskOwners struct {
	mu     sync.Mutex
	owners map[string]string
	// after schedules a forget; a test replaces it to run it synchronously.
	after func(time.Duration, func())
}

func newTaskOwners() *taskOwners {
	return &taskOwners{owners: map[string]string{}, after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
}

// addHooks records each task's owner at creation, refuses tasks/get,
// tasks/result and tasks/cancel for a task the caller does not own, and
// filters tasks/list to the caller's own.
func (t *taskOwners) addHooks(hooks *mcpserver.Hooks, taskHooks *mcpserver.TaskHooks) {
	taskHooks.AddOnTaskCreated(func(ctx context.Context, m mcpserver.TaskMetrics) {
		t.mu.Lock()
		t.owners[m.TaskID] = callerOwner(ctx)
		t.mu.Unlock()
	})
	finished := func(_ context.Context, m mcpserver.TaskMetrics) {
		t.after(taskOwnerRetention, func() {
			t.mu.Lock()
			delete(t.owners, m.TaskID)
			t.mu.Unlock()
		})
	}
	taskHooks.AddOnTaskCompleted(finished)
	taskHooks.AddOnTaskFailed(finished)
	taskHooks.AddOnTaskCancelled(finished)

	// The one hook that runs before every request AND can refuse it.
	//
	// It FAILS CLOSED: a request it cannot read is refused, never waved
	// through. mcp-go parses the same bytes after this hook, and a request this
	// hook could not read may still be one mcp-go reads (a different decoder, a
	// future field type); refusing it here is what keeps a task request from
	// reaching mcp-go unchecked.
	hooks.AddOnRequestInitialization(func(ctx context.Context, _ any, message any) error {
		raw, ok := message.(json.RawMessage)
		if !ok {
			var err error
			if raw, err = json.Marshal(message); err != nil {
				return errors.New(errUnreadableRequest)
			}
		}
		var envelope struct {
			Method mcpgo.MCPMethod `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			return errors.New(errUnreadableRequest)
		}
		switch envelope.Method {
		case mcpgo.MethodTasksGet, mcpgo.MethodTasksResult, mcpgo.MethodTasksCancel:
			var params struct {
				TaskID string `json:"taskId"`
			}
			if json.Unmarshal(envelope.Params, &params) != nil || !t.owns(ctx, params.TaskID) {
				return errors.New(errTaskNotFound)
			}
		}
		return nil
	})
	hooks.AddAfterListTasks(func(ctx context.Context, _ any, _ *mcpgo.ListTasksRequest, result *mcpgo.ListTasksResult) {
		if result == nil {
			return
		}
		kept := result.Tasks[:0:0]
		for _, task := range result.Tasks {
			if t.owns(ctx, task.TaskId) {
				kept = append(kept, task)
			}
		}
		result.Tasks = kept
	})
}

// owns reports whether the caller on ctx owns task id. An id with no recorded
// owner — never created, or forgotten — is owned by nobody.
func (t *taskOwners) owns(ctx context.Context, id string) bool {
	t.mu.Lock()
	owner, ok := t.owners[id]
	t.mu.Unlock()
	return ok && owner == callerOwner(ctx)
}
