// knomit_experiment: the lifecycle of an `exp/<name>` branch, from the agent's
// side.
//
// Nothing here is special-cased per tool. Opening an experiment moves where
// the CALLER is, and every other tool then works on it unchanged — that is the
// whole design, and the reason this file is about the binding and not about
// facts.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// experimentTool declares knomit_experiment.
//
// The description carries the two things an agent cannot discover by trying:
// that `open` MOVES this session (on the unscoped mount) rather than just
// creating a branch, and that a commit can be REFUSED with a list of paths,
// which is a state it has to know how to leave.
func experimentTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_experiment",
		mcpgo.WithDescription("Work on an isolated branch of this knowledge base. `open` forks exp/<name> from the agent branch and MOVES THIS SESSION onto it: every later call — learn, update, retract, query, review — reads and writes the experiment until you commit or roll back. `commit` merges it into the agent branch and deletes it; if both sides changed the same fact it is REFUSED with the conflicting paths and nothing changes — call `sync` (which brings the agent branch's version onto the experiment, agent wins) and commit again, or `rollback` to throw the work away. `list` shows this repo's experiments and marks the one you are in. Experiments are local: never pushed, never fetched, never visible to a peer. An experiment with no commits for the configured expiry is rolled back automatically."),
		mcpgo.WithString("action", mcpgo.Required(),
			mcpgo.Description("One of: list, open, commit, rollback, sync.")),
		mcpgo.WithString("name",
			mcpgo.Description("The experiment name: kebab-case, unique in this repo (e.g. \"widen-the-gate\"). Required for open; for commit/rollback/sync it defaults to the experiment you are currently in.")),
		mcpgo.WithString("description",
			mcpgo.Description("Free text saying what this experiment is for. Only used by `open`; stored locally, never committed to git. Re-opening with a new description replaces it; re-opening with none keeps it.")),
		bindingArg(true),
	)
}

// experimentResult is the wire shape. Every field is omitempty except action,
// because one result type serves five actions and a caller should not have to
// read empty structure to find the part that applies to it.
type experimentResult struct {
	Action string `json:"action"`
	// Repo and Branch say WHERE this happened, for the same reason
	// writeDestination exists on the write tools: an experiment opened on the
	// wrong base looks identical to one opened on the right base.
	Repo   string `json:"repo"`
	RepoID string `json:"repo_id,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Active is the experiment this session is in AFTER the call.
	Active      string           `json:"active_experiment,omitempty"`
	Experiments []experimentView `json:"experiments,omitempty"`
	// ReconnectURL is set when the action could not move the caller because
	// the mount is bound by its URL. Absent on the session-scoped mount.
	ReconnectURL string `json:"reconnect_url,omitempty"`
	// A refused commit is NOT a field here: it is a tool ERROR, because the
	// action did not happen, and the conflicting paths are named in that
	// error's text alongside the two ways out. A success-shaped result
	// carrying a "conflicts" list would be read as "committed, with notes".
	Summary string `json:"summary"`
}

// experimentView is one row of `list`.
type experimentView struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Branch       string `json:"branch"`
	Parent       string `json:"parent"`
	ForkCommit   string `json:"fork_commit"`
	LastActivity string `json:"last_activity"`
	// Current marks the one this session is inside, which is the question a
	// caller actually has when it calls list.
	Current bool `json:"current,omitempty"`
}

// ExperimentHandler returns the handler for knomit_experiment.
func ExperimentHandler(mgr *repos.Manager) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, experimentTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		ri := b.Write()
		svc, release, err := ri.Acquire()
		if err != nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}
		defer release()

		action := req.GetString("action", "")
		name := req.GetString("name", "")
		active := repos.ActiveExperiment(b)

		var (
			res  experimentResult
			rerr error
		)
		switch action {
		case "list":
			res, rerr = experimentList(ctx, svc, b, active)
		case "open":
			res, rerr = experimentOpen(ctx, mgr, svc, b, name, req.GetString("description", ""), active)
		case "commit":
			res, rerr = experimentCommit(ctx, mgr, svc, b, pickExperiment(name, active))
		case "rollback":
			res, rerr = experimentRollback(ctx, mgr, svc, b, pickExperiment(name, active))
		case "sync":
			res, rerr = experimentSync(ctx, svc, b, pickExperiment(name, active))
		case "":
			return mcpgo.NewToolResultError(
				"knomit_experiment needs an `action`: one of list, open, commit, rollback, sync"), nil
		default:
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"unknown action %q — knomit_experiment takes one of list, open, commit, rollback, sync", action)), nil
		}
		if rerr != nil {
			return mcpgo.NewToolResultError(rerr.Error()), nil
		}

		res.Action = action
		res.Repo = ri.Name()
		res.RepoID = ri.ShortID()
		out, merr := json.MarshalIndent(res, "", "  ")
		if merr != nil {
			return mcpgo.NewToolResultError("marshal error: " + merr.Error()), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}

// pickExperiment resolves which experiment an action addresses: the explicit
// name, else the one the caller is in. Returning "" lets each action say what
// IT needs, which is better than one generic "name required" that cannot
// mention the active-experiment fallback.
func pickExperiment(name, active string) string {
	if name != "" {
		return name
	}
	return active
}

// experimentList reports this repo's experiments, marking the current one.
func experimentList(ctx context.Context, svc *store.Service, b *repos.Binding, active string) (experimentResult, error) {
	all, err := svc.Experiments().ListExperiments(ctx)
	if err != nil {
		return experimentResult{}, err
	}
	views := make([]experimentView, 0, len(all))
	for _, e := range all {
		views = append(views, experimentView{
			Name:         e.Name,
			Description:  e.Description,
			Branch:       e.Branch(),
			Parent:       e.Parent,
			ForkCommit:   e.ForkCommit,
			LastActivity: e.LastActivityAt.UTC().Format(time.RFC3339),
			Current:      e.Name == active,
		})
	}
	summary := fmt.Sprintf("%d experiment(s) on %q", len(views), b.Write().Name())
	if active != "" {
		summary += fmt.Sprintf("; this session is inside %q", active)
	} else {
		summary += fmt.Sprintf("; this session is on %q", b.WriteBranch())
	}
	return experimentResult{
		Branch: b.WriteBranch(), Active: active, Experiments: views, Summary: summary,
	}, nil
}

// experimentOpen forks or resumes, and then moves the caller onto it when the
// mount can be moved.
func experimentOpen(ctx context.Context, mgr *repos.Manager, svc *store.Service, b *repos.Binding, name, description, active string) (experimentResult, error) {
	ri := b.Write()
	if name == "" {
		return experimentResult{}, errors.New("knomit_experiment open needs a `name`: kebab-case, unique in this repo")
	}
	// The refusals come BEFORE the fork, and each names the reason rather
	// than letting a later WritableBranch failure present as a mysterious
	// read-only view.
	if ri.Subscribed() {
		return experimentResult{}, fmt.Errorf(
			"repo %q is a subscription: it follows a remote branch read-only and has no agent branch to fork an experiment from",
			ri.Name())
	}
	if oerr := ri.OntologyError(); oerr != nil {
		return experimentResult{}, fmt.Errorf(
			"repo %q has no established ontology, so it accepts no writes on any branch, experiments included (%v)",
			ri.Name(), oerr)
	}
	parent := ri.AgentBranch()
	if !ri.WritableBranch(parent) {
		return experimentResult{}, fmt.Errorf(
			"repo %q cannot author on its own agent branch %q, so it cannot fork an experiment from it",
			ri.Name(), parent)
	}

	exp, err := svc.Experiments().OpenExperiment(ctx, name, description, parent)
	if err != nil {
		return experimentResult{}, err
	}

	res := experimentResult{Branch: exp.Branch(), Active: exp.Name}

	// Moving the caller is only possible where the caller is named by a
	// handle. On a URL-scoped mount the endpoint IS the selection, so the
	// experiment exists but this session is still on the old branch — and
	// saying so plainly matters more than the refusal would: an agent told
	// only "created" will keep writing to the agent branch believing
	// otherwise.
	handle := bindingHandleFromContext(ctx)
	if handle == "" {
		res.ReconnectURL = experimentReconnectURL(b, exp.Name)
		// Active is where this session IS, not what was just created — and on
		// a URL-scoped mount those differ, which is the whole point of this
		// branch. A caller already inside exp/a that opens exp/b is still in
		// a; reporting "" would tell it it had left.
		res.Active = active
		res.Summary = fmt.Sprintf(
			"experiment %q is ready on branch %q, but THIS SESSION IS NOT INSIDE IT: this endpoint is bound by its URL. "+
				"Reconnect to %s to work in it. Until then every write still goes to %q.",
			exp.Name, exp.Branch(), res.ReconnectURL, b.WriteBranch())
		return res, nil
	}
	if err := setHandleExperiment(ctx, mgr, handle, exp.Name); err != nil {
		return experimentResult{}, err
	}
	res.Summary = fmt.Sprintf(
		"experiment %q is open on branch %q and THIS SESSION IS NOW INSIDE IT: every later call reads and writes there until you commit or roll back.",
		exp.Name, exp.Branch())
	return res, nil
}

// experimentCommit merges into the agent branch and leaves the experiment.
func experimentCommit(ctx context.Context, mgr *repos.Manager, svc *store.Service, b *repos.Binding, name string) (experimentResult, error) {
	if name == "" {
		return experimentResult{}, errors.New(
			"knomit_experiment commit needs a `name`, or a session that is inside an experiment")
	}
	result, err := svc.Experiments().CommitExperiment(ctx, name)
	if err != nil {
		// A refused commit is a STATE, not a malfunction, so it comes back as
		// the paths plus the two ways out rather than as a bare error string.
		var conflict *store.MergeConflictError
		if errors.As(err, &conflict) {
			return experimentResult{}, fmt.Errorf(
				"commit refused: %s and %s both changed %d path(s) since the fork, and nothing was changed. "+
					"Paths: %v. Run knomit_experiment {action: \"sync\"} to take the agent branch's version of them "+
					"onto the experiment, then commit again — or rollback to discard the experiment",
				conflict.Src, conflict.Dst, len(conflict.Paths), conflict.Paths)
		}
		return experimentResult{}, err
	}
	// Eager clear for the caller's own handle, so its very next call is back
	// on the agent branch without a round trip. Every OTHER handle pointing at
	// this experiment heals on its next resolution instead — see
	// repos.ResolveSessionBindingOnExperiment.
	clearHandleExperiment(ctx, mgr, bindingHandleFromContext(ctx))
	return experimentResult{
		Branch: b.Write().AgentBranch(),
		Summary: fmt.Sprintf(
			"experiment %q merged into %q (%s) and deleted; this session is back on %q",
			name, b.Write().AgentBranch(), result.Mode, b.Write().AgentBranch()),
	}, nil
}

// experimentRollback discards the experiment.
func experimentRollback(ctx context.Context, mgr *repos.Manager, svc *store.Service, b *repos.Binding, name string) (experimentResult, error) {
	if name == "" {
		return experimentResult{}, errors.New(
			"knomit_experiment rollback needs a `name`, or a session that is inside an experiment")
	}
	if err := svc.Experiments().RollbackExperiment(ctx, name); err != nil {
		return experimentResult{}, err
	}
	clearHandleExperiment(ctx, mgr, bindingHandleFromContext(ctx))
	return experimentResult{
		Branch: b.Write().AgentBranch(),
		Summary: fmt.Sprintf(
			"experiment %q and everything on it are gone; this session is back on %q",
			name, b.Write().AgentBranch()),
	}, nil
}

// experimentSync brings the agent branch's version of conflicting paths onto
// the experiment. It does NOT clear the caller's experiment: syncing is how
// you stay in one.
func experimentSync(ctx context.Context, svc *store.Service, b *repos.Binding, name string) (experimentResult, error) {
	if name == "" {
		return experimentResult{}, errors.New(
			"knomit_experiment sync needs a `name`, or a session that is inside an experiment")
	}
	result, err := svc.Experiments().SyncExperiment(ctx, name)
	if err != nil {
		return experimentResult{}, err
	}
	return experimentResult{
		Branch: store.ExperimentBranch(name), Active: name,
		Summary: fmt.Sprintf(
			"experiment %q updated from %q (%s); where both had changed a fact, the agent branch's version won. "+
				"You are still inside the experiment",
			name, b.Write().AgentBranch(), result.Mode),
	}, nil
}

// experimentReconnectURL is the endpoint a URL-scoped caller should reconnect
// to in order to work inside name.
//
// The two mounts are mirrored surfaces, so both spellings live here together
// (kb/architecture/web/096bb34b): a repo mount carries the experiment as its
// BRANCH segment, in the ":"-for-"/" form the branch middleware decodes, and a
// lens mount carries it as its own segment because a lens pins a branch per
// member and has no single branch to put in a path.
// The "/api/v1" prefix is spelled here rather than imported: internal/web
// imports internal/mcp, so the dependency cannot run the other way. Both
// spellings are pinned against the REAL router by
// web.TestExperiment_URLScopedOpenReturnsTheReconnectURL, which calls the URL
// this returns and asserts it lands inside the experiment, and by
// web.TestLensExperimentMount_RePinsOnlyTheWriteMember for the lens form. A
// string that drifted from the route would fail there rather than in
// production.
func experimentReconnectURL(b *repos.Binding, name string) string {
	if b.FromLens() {
		return fmt.Sprintf("/api/v1/lenses/%s/experiments/%s/mcp", b.Name(), name)
	}
	return fmt.Sprintf("/api/v1/repos/%s/branches/exp:%s/mcp", b.Write().Name(), name)
}

// setHandleExperiment moves the caller's handle into an experiment.
//
// A failure here is REPORTED, not swallowed: the branch exists at this point,
// so a caller told "you are inside it" that is not inside it would write its
// next fact to the agent branch.
func setHandleExperiment(ctx context.Context, mgr *repos.Manager, handle, name string) error {
	if mgr == nil {
		return errStoreUnavailable
	}
	st := mgr.ClientSessions()
	if st == nil {
		return errors.New("session store unavailable — the experiment exists, but this session could not be moved into it")
	}
	if err := st.SetHandleExperiment(ctx, handle, name, time.Now()); err != nil {
		log.Warn().Err(err).Str("experiment", name).
			Msg("knomit_experiment: recording the session's experiment failed")
		return errors.New("the experiment exists, but this session could not be moved into it; ask the operator to check the server log")
	}
	return nil
}

// clearHandleExperiment puts the caller's handle back on the agent branch.
//
// BEST EFFORT, and the asymmetry with setHandleExperiment is deliberate. By
// the time this runs the experiment is already gone from the store, so a
// handle still naming it resolves to "no such experiment" and heals itself on
// the very next call. Failing a successful commit over bookkeeping that
// self-corrects would be the worse trade.
func clearHandleExperiment(ctx context.Context, mgr *repos.Manager, handle string) {
	if handle == "" || mgr == nil {
		return
	}
	st := mgr.ClientSessions()
	if st == nil {
		return
	}
	if err := st.ClearHandleExperiment(ctx, handle); err != nil {
		log.Warn().Err(err).Msg("knomit_experiment: clearing the session's experiment failed; it will heal on the next call")
	}
}
