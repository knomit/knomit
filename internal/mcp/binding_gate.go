package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
)

// bindingArgument is the name of the opaque handle every binding-requiring tool
// takes. One spelling, used by the tool schemas, the gate and the instructions.
const bindingArgument = "binding"

// Error texts the gate returns. They are constants because they are a CONTRACT
// with the agent — the tests assert the text, and an agent that cannot tell
// "you forgot the handle" from "that handle is not one of mine" will retry the
// wrong repair forever.
const (
	// errUnknownHandle deliberately says nothing about which handles exist.
	// The handle is the only thing distinguishing two callers on one
	// connection, so enumerating live handles in an error would hand one
	// caller another's routing.
	errUnknownHandle = "unknown binding handle — call knomit_bind"
	// errBindingOnURLScoped answers a `binding` on a --repo/--lens endpoint.
	// Silently ignoring it would be worse than failing: the caller believes it
	// selected a repo, and the URL picked a different one.
	errBindingOnURLScoped = "this endpoint is bound by its URL; do not pass binding"
)

// errMissingHandle is the unscoped endpoint's "you did not pass one". It names
// the tool that mints a handle and the argument that carries it, because that
// pair is the entire repair.
func errMissingHandle(tool string) string {
	return fmt.Sprintf(
		"%s requires a `binding` argument: the opaque handle knomit_bind returned. "+
			"Call knomit_bind with a repo or lens name, then pass its `binding` value on this and every "+
			"later tool call. There is no session fallback — the handle is the only thing that says "+
			"which knowledge base this call is for.", tool)
}

// bindingArg is the `binding` parameter, declared identically by every tool the
// gate protects. Declaring it matters twice over: rejectUnknownArguments
// derives its valid set from the tool's own schema, and the description is
// where an agent reading tools/list learns the handle exists at all.
func bindingArg(required bool) mcpgo.ToolOption {
	desc := "The opaque handle returned by knomit_bind. REQUIRED on the unscoped /api/v1/mcp " +
		"endpoint; must NOT be passed on a URL-scoped endpoint (a bridge started with --repo or " +
		"--lens), which is bound by its URL. Pass the value verbatim — it is random, it is not the " +
		"repo or lens name, and an invented one is refused."
	if !required {
		desc = "Optional. " + desc + " Without it, this tool reports the server catalogue with no `bound` section."
	}
	return mcpgo.WithString(bindingArgument, mcpgo.Description(desc))
}

// gateMode says what the gate does with the handle for one tool.
type gateMode int

const (
	// gateRequired: the seven tools that operate on a knowledge base. No
	// handle, or an unrecognised one, is a tool error and the call never runs.
	gateRequired gateMode = iota
	// gateUngated: knomit_bind alone. It mints handles, so requiring one would
	// be circular, and it validates the session-scoped marker itself.
	gateUngated
	// gateOptional: knomit_repos, the discovery tool. It must answer while
	// nothing is bound — that is how an agent learns the names knomit_bind
	// accepts — so a missing handle passes through and a bad one is REPORTED
	// (bound.status: "unresolvable") rather than raised, because this is
	// precisely the tool an agent calls to find out what went wrong.
	gateOptional
)

// gateBinding wraps a tool handler with the handle gate.
//
// WHY A WRAPPER AT REGISTRATION, and not the two seams that look more natural:
//
//   - A server.Hooks BeforeCallTool hook returns nothing. It can observe the
//     call but cannot hand the handler a different context, so it cannot put
//     the resolved Binding where every handler already reads it.
//   - server.WithToolHandlerMiddleware CAN rewrite the context — but mcp-go
//     v0.45 applies the middleware chain only on the synchronous path.
//     handleTaskAugmentedToolCall calls tool.Handler directly
//     (executeTaskTool / executeRegularToolAsTask), so a task-augmented call
//     to knomit_review or knomit_hypothesize — both of which declare
//     TaskSupportOptional — would bypass the gate entirely.
//
// Wrapping the handler before AddTool is the one seam BOTH paths go through,
// because both of them call exactly the handler that was registered.
//
// The handler bodies are untouched: the gate leaves the context in the shape
// ResolveSessionBinding always produced — the Binding plus the write repo — so
// repos.RequireBinding answers as it did when a middleware resolved it.
func gateBinding(mgr *repos.Manager, tool mcpgo.Tool, mode gateMode, next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		// A non-object `arguments` is answered here with the handler's own
		// message rather than being read as an absent handle: "expected a JSON
		// object" is actionable and "you forgot the binding" is not.
		if err := rejectNonObjectArguments(req, tool); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		raw, passed := req.GetArguments()[bindingArgument]
		handle, isString := raw.(string)

		// A URL-scoped endpoint gets its repo from the path. A handle there is
		// meaningless at best and contradicts the URL at worst, so it is
		// refused on PRESENCE — an empty string is still a caller that thinks
		// it is selecting something.
		if !repos.SessionScoped(ctx) {
			if passed {
				return mcpgo.NewToolResultError(errBindingOnURLScoped), nil
			}
			return next(ctx, req)
		}

		// A handle of the wrong TYPE is named as such. Folded into "you forgot
		// it", a caller that sent one would read the advice as already
		// followed and resend the same shape.
		if passed && !isString {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"`binding` must be the handle string knomit_bind returned, got %T", raw)), nil
		}
		if handle == "" {
			if mode == gateOptional {
				// knomit_repos with nothing bound: the catalogue, no `bound`.
				return next(ctx, req)
			}
			return mcpgo.NewToolResultError(errMissingHandle(tool.Name)), nil
		}

		ctx, err := resolveHandle(ctx, mgr, handle)
		if err != nil {
			if mode == gateOptional {
				// Report, do not raise: knomit_repos renders this as
				// bound.status "unresolvable" with the reason.
				return next(repos.WithBindingError(ctx, err), req)
			}
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		return next(ctx, req)
	}
}

// resolveHandle turns a handle into a context carrying its Binding, and records
// the pin for the observational client_sessions column.
//
// The two failures are kept distinct on purpose. A handle nobody minted is the
// agent's own mistake and says only "call knomit_bind". A handle that WAS
// minted but whose repo has since gone surfaces the resolver's own reason,
// because "bind again" is useless advice if the agent cannot tell that the base
// it chose no longer exists.
func resolveHandle(ctx context.Context, mgr *repos.Manager, handle string) (context.Context, error) {
	if mgr == nil {
		return ctx, errStoreUnavailable
	}
	store := mgr.ClientSessions()
	if store == nil {
		return ctx, errors.New("session store unavailable — this server cannot resolve a binding handle right now")
	}
	pin, branch, ok, err := store.BindingHandle(ctx, handle, time.Now())
	if err != nil {
		log.Warn().Err(err).Msg("binding gate: handle lookup failed")
		return ctx, errors.New("binding handle lookup failed — retry, or call knomit_bind again")
	}
	if !ok {
		return ctx, errors.New(errUnknownHandle)
	}
	bctx, rerr := repos.ResolveSessionBinding(ctx, mgr, pin, branch)
	if rerr != nil {
		return ctx, rerr
	}
	// Observational only: the client_sessions row and the session's binding SET
	// record what this session was seen doing. Routing was already decided, by
	// the handle. The handle rides along because the set is keyed by it.
	if rec, found := repos.PinRecorderFromContext(bctx); found {
		rec.Record(repos.ResolvedBinding{Handle: handle, Pin: pin, Branch: branch})
	}
	return bctx, nil
}
