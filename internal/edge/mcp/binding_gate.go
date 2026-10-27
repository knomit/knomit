package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
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
	// errHandleFromScopedEndpoint answers, on the unscoped endpoint, a handle
	// that knomit_bind minted on a URL-scoped one. Such a handle names a
	// branch within one --repo/--lens connection and means nothing here.
	errHandleFromScopedEndpoint = "this binding handle was minted on a URL-scoped endpoint (a --repo or --lens bridge) and works only there — call knomit_bind on this endpoint"
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

// bindingArg is the `binding` parameter, declared by every tool the gate
// protects. Declaring it matters twice over: rejectUnknownArguments derives its
// valid set from the tool's own schema, and the description is where an agent
// reading tools/list learns the handle exists at all.
//
// The registered text is the UNSCOPED endpoint's. A URL-scoped endpoint serves
// the same tools with a different text, swapped in per tools/list by
// endpointToolFilter, because there the handle is optional and absence means
// something. Each (endpoint kind, gate mode) pair has its own text rather than
// one built from another: an "Optional." stuck in front of the required text
// once told knomit_repos callers both things at once.
// TestBindingArg_DescriptionMatchesGateMode pins every pair.
func bindingArg(required bool) mcpgo.ToolOption {
	mode := gateOptional
	if required {
		mode = gateRequired
	}
	return mcpgo.WithString(bindingArgument, mcpgo.Description(bindingDescription(mode, false)))
}

// bindingDescription is the `binding` text for one gate mode on one endpoint
// kind. "" for gateUngated, which declares no `binding`.
func bindingDescription(mode gateMode, scoped bool) string {
	switch {
	case mode == gateRequired && !scoped:
		return bindingRequiredDescription
	case mode == gateOptional && !scoped:
		return bindingOptionalDescription
	case mode == gateRequired && scoped:
		return bindingScopedDescription
	case mode == gateOptional && scoped:
		return bindingScopedReposDescription
	}
	return ""
}

const (
	// bindingRequiredDescription is gateRequired's `binding` on the unscoped
	// endpoint.
	bindingRequiredDescription = "The opaque handle returned by knomit_bind. REQUIRED on this endpoint: it " +
		"says which knowledge base the call is for. Pass the value verbatim — it is random, it is not the " +
		"repo or lens name, and an invented one, or one minted on another endpoint, is refused. An empty " +
		"string counts as not passing one."
	// bindingOptionalDescription is gateOptional's `binding` (knomit_repos) on
	// the unscoped endpoint: a missing handle passes, a bad one is reported in
	// `bound`.
	bindingOptionalDescription = "Optional: a handle returned by knomit_bind. With it, the result also has a " +
		"`bound` section describing what the handle names; a handle that does not resolve is reported there " +
		"(status \"unresolvable\"), not raised. Without it, or with \"\", there is no `bound` section."
	// bindingScopedDescription is gateRequired's `binding` on a URL-scoped
	// endpoint, where the handle only chooses a branch.
	bindingScopedDescription = "Optional on this endpoint, which is fixed to one repo or lens by its URL. " +
		"Omit it (or pass \"\") to use that repo or lens at its default branch, or inside the experiment " +
		"this connection opened. Pass a handle that knomit_bind returned on THIS connection to work on the " +
		"branch or experiment that handle names. A handle from another endpoint is refused."
	// bindingScopedReposDescription is gateOptional's `binding` (knomit_repos)
	// on a URL-scoped endpoint.
	bindingScopedReposDescription = "Optional: a handle that knomit_bind returned on this connection. With it, " +
		"`bound` describes what the handle names; a handle that does not resolve is reported there (status " +
		"\"unresolvable\"), not raised. Without it, or with \"\", `bound` describes this connection's own " +
		"repo or lens."
)

// gateMode says what the gate does with the handle for one tool.
type gateMode int

const (
	// gateRequired: every tool that operates on a knowledge base. No
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

		// A handle of the wrong TYPE is named as such, on both endpoint kinds.
		// Folded into "you forgot it", a caller that sent one would read the
		// advice as already followed and resend the same shape.
		if passed && !isString {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"`binding` must be the handle string knomit_bind returned, got %T", raw)), nil
		}

		// A URL-scoped endpoint takes its repo or lens from the path. There the
		// handle is OPTIONAL: absent (or "") is the URL's own binding, exactly
		// as before handles existed there; present routes the call by the
		// handle, which can name only a branch within that same repo or lens.
		scoped := !repos.SessionScoped(ctx)

		if handle == "" {
			if scoped || mode == gateOptional {
				// Scoped: the URL's binding. knomit_repos unscoped: the
				// catalogue, no `bound`.
				return next(ctx, req)
			}
			return mcpgo.NewToolResultError(errMissingHandle(tool.Name)), nil
		}

		// The endpoint the handle must have been minted on: "" for the
		// unscoped endpoint, the mount's pin for a scoped one.
		mount := ""
		if scoped {
			_, uid, plain := repos.MountSessionFromContext(ctx)
			if !plain {
				// The URL names a specific branch or experiment, so nothing
				// here can choose one: knomit_bind refuses on this mount too.
				err := errors.New(errFixedEndpoint(ctx))
				if mode == gateOptional {
					return next(repos.WithBindingError(ctx, err), req)
				}
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			mount = uid
		}

		rctx, err := resolveHandle(ctx, mgr, handle, mount)
		if err != nil {
			if mode == gateOptional {
				// Report, do not raise: knomit_repos renders this as
				// bound.status "unresolvable" with the reason.
				return next(repos.WithBindingError(ctx, err), req)
			}
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		return next(rctx, req)
	}
}

// errHandleFromOtherEndpoint answers, on a URL-scoped endpoint, a handle that
// was minted anywhere but this connection's own mount.
func errHandleFromOtherEndpoint(ctx context.Context) string {
	kind, name := urlTarget(ctx)
	return fmt.Sprintf("this binding handle belongs to another endpoint — omit `binding` to use this connection's %s %q, "+
		"or call knomit_bind on this connection for a handle", kind, name)
}

// errFixedEndpoint answers a handle, or a knomit_bind, on a URL-scoped mount
// whose URL names a branch other than the repo's own agent branch (main, a
// foreign agent branch, an experiment). Such a URL is an ADDRESS, and nothing
// may move it (kb/invariants/mcp/experiments/mount-eligibility-from-the-route).
func errFixedEndpoint(ctx context.Context) string {
	kind, name := urlTarget(ctx)
	return fmt.Sprintf("this endpoint is fixed to %s %q at branch %q by its URL, so no handle can change its branch — omit `binding`. Do not retry.",
		kind, name, fixedBranch(ctx))
}

// urlTarget names what a URL-scoped request's URL fixes: ("repo"|"lens", name).
func urlTarget(ctx context.Context) (kind, name string) {
	if b, ok := repos.BindingFromContextOpt(ctx); ok {
		if b.FromLens() {
			return "lens", b.Name()
		}
		if w := b.Write(); w != nil {
			return "repo", w.Name()
		}
	}
	if ri, ok := repos.RepoFromContextOpt(ctx); ok {
		return "repo", ri.Name()
	}
	return "repo", ""
}

// fixedBranch is the branch a URL-scoped request's URL names, or the branch its
// write repo is served at when the route has no {branch} segment.
func fixedBranch(ctx context.Context) string {
	if br, ok := repos.BranchFromContextOpt(ctx); ok && br != "" {
		return br
	}
	if b, ok := repos.BindingFromContextOpt(ctx); ok {
		if w := b.WriteBranch(); w != "" {
			return w
		}
		if b.Write() != nil {
			return b.Write().ReadBranch()
		}
	}
	return ""
}

// resolveHandle turns a handle into a context carrying its Binding, and records
// the pin for the observational client_sessions column.
//
// The two failures are kept distinct on purpose. A handle nobody minted is the
// agent's own mistake and says only "call knomit_bind". A handle that WAS
// minted but whose repo has since gone surfaces the resolver's own reason,
// because "bind again" is useless advice if the agent cannot tell that the base
// it chose no longer exists.
//
// mount is the endpoint the call arrived on: "" for the unscoped endpoint, the
// mount's pin for a URL-scoped one. A handle works only on the endpoint that
// minted it, so a handle from the unscoped endpoint cannot steer a --repo
// bridge and a --repo bridge's handle cannot be replayed on the unscoped one.
func resolveHandle(ctx context.Context, mgr *repos.Manager, handle, mount string) (context.Context, error) {
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
	minted, merr := store.HandleMount(ctx, handle)
	if merr != nil {
		log.Warn().Err(merr).Msg("binding gate: handle mount lookup failed")
		return ctx, errors.New("binding handle lookup failed — retry, or call knomit_bind again")
	}
	if minted != mount {
		if mount == "" {
			return ctx, errors.New(errHandleFromScopedEndpoint)
		}
		return ctx, errors.New(errHandleFromOtherEndpoint(ctx))
	}
	if mount != "" {
		// Unreachable while knomit_bind mints scoped handles with the mount's
		// own pin. Checked anyway: a handle naming another base here would be
		// the 2026-09-17 incident on a scoped endpoint.
		if pin != mount {
			return ctx, errors.New(errHandleFromOtherEndpoint(ctx))
		}
		// The HANDLE routes this call, so the connection's session-keyed
		// experiment must not touch it: neither apply to it nor be opened,
		// committed or cleared by it.
		ctx = repos.WithoutMountState(ctx)
	}
	// The experiment this handle is working inside, if any. Read here rather
	// than deeper because this is where the handle is known, and the handle
	// is the key — the MCP session id is NOT, and must never become one
	// (kb/invariants/mcp/session-binding/handle-is-the-only-router).
	//
	// A FAILED read is a hard error, not an empty answer: "" means "write to
	// the agent branch", so swallowing the error would silently take a caller
	// out of the experiment it is working in and land its next fact somewhere
	// it did not choose.
	experiment, eerr := store.HandleExperiment(ctx, handle)
	if eerr != nil {
		log.Warn().Err(eerr).Msg("binding gate: handle experiment lookup failed")
		return ctx, errors.New("binding handle lookup failed — retry, or call knomit_bind again")
	}
	bctx, rerr := repos.ResolveSessionBindingOnExperiment(ctx, mgr, pin, branch, experiment)
	if rerr != nil {
		return ctx, rerr
	}
	// The handle rides in the context so the experiment tools can clear the
	// caller's OWN experiment on commit and rollback. Nothing routes off it —
	// routing was decided above — and only the tool that already holds the
	// handle can use it.
	bctx = withBindingHandle(bctx, handle)
	// Observational only: the client_sessions row and the session's binding SET
	// record what this session was seen doing. Routing was already decided, by
	// the handle. The handle rides along because the set is keyed by it.
	if rec, found := repos.PinRecorderFromContext(bctx); found {
		rec.Record(repos.ResolvedBinding{Handle: handle, Pin: pin, Branch: branch})
	}
	return bctx, nil
}

// bindingHandleKey is the private context key for the handle that resolved
// this request.
type bindingHandleKey struct{}

// withBindingHandle stores the resolving handle. Set only on the
// session-scoped mount, where a handle exists at all.
func withBindingHandle(ctx context.Context, handle string) context.Context {
	return context.WithValue(ctx, bindingHandleKey{}, handle)
}

// bindingHandleFromContext returns the handle that resolved this request, or
// "" on a URL-scoped mount — where there is no handle by design, and where an
// experiment is named by the URL instead.
//
// This is NOT a routing input. It exists so knomit_experiment can clear the
// caller's own experiment eagerly at commit and rollback; every other handle
// pointing at the same experiment heals on its next resolution instead.
func bindingHandleFromContext(ctx context.Context) string {
	h, _ := ctx.Value(bindingHandleKey{}).(string)
	return h
}
