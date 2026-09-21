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
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/resolutions"
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
		mcpgo.WithDescription("Work on an isolated branch of this knowledge base. `open` forks exp/<name> from the agent branch and MOVES THIS SESSION onto it: every later call — learn, update, retract, query, review — reads and writes the experiment until you commit or roll back. `commit` merges it into the agent branch and deletes it. If both sides changed the same fact the commit is REFUSED, nothing changes, and you are told the conflicting paths plus THREE COMMITS for each: the fork point, the experiment's version, and the agent branch's version. Read the fact at all three with knomit_explain {file, commit}, decide per path, and retry `commit` with `resolutions`. `rollback` throws the work away. `list` shows this repo's experiments and marks the one you are in. Experiments are local: never pushed, never fetched, never visible to a peer. An experiment with no commits for the configured expiry is rolled back automatically.\n\nOURS AND THEIRS: `commit` merges the EXPERIMENT INTO the agent branch, so the experiment is the merge SOURCE. From where you are sitting — inside the experiment — \"ours\" is the experiment's version and \"theirs\" is the agent branch's. That is the OPPOSITE of git's own merge convention, where \"ours\" is the branch being merged into. If you are used to git, read these two words carefully.\n\nRESOLVING: when the merge is obvious — a fact you wrote this session, or two edits that plainly compose — resolve it yourself and commit. When the two versions disagree about a CLAIM rather than its wording, or the other version is someone else's work, show the human ours, theirs and your proposed merge, and commit with their picks. That choice is yours to make; there is no flag for it."),
		mcpgo.WithString("action", mcpgo.Required(),
			mcpgo.Description("One of: list, open, commit, rollback, sync.")),
		mcpgo.WithString("name",
			mcpgo.Description("The experiment name: kebab-case, unique in this repo (e.g. \"widen-the-gate\"). Required for open; for commit/rollback/sync it defaults to the experiment you are currently in.")),
		mcpgo.WithObject("resolutions",
			mcpgo.Description("Only for `commit`, and only after one was refused: how to settle each conflicting path, keyed by the fact path exactly as the refusal listed it. Each value is \"ours\" (keep THIS EXPERIMENT's version — it is the merge source, the opposite of git's \"ours\"), \"theirs\" (take the agent branch's version), or {\"body\": \"<full merged fact text>\"} to land content that is neither. Every path in the refusal needs an entry — one left out is refused again, and a path that did not conflict is an error rather than a no-op. The merge is still ONE merge: everything else merges exactly as it would have.")),
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
	// A refused commit is NOT a field here: it is a tool ERROR, because the
	// action did not happen, and the conflicting paths are named in that
	// error's text alongside the two ways out. A success-shaped result
	// carrying a "conflicts" list would be read as "committed, with notes".
	Summary string `json:"summary"`
}

// parseResolutions turns the tool's `resolutions` argument into store
// resolutions, keyed by fact path.
//
// OURS AND THEIRS ARE TRANSLATED HERE, once, and this is the only place in the
// codebase entitled to use those words. Commit merges the experiment INTO the
// parent, so the experiment is the merge SOURCE: "ours" (the agent's own work,
// in the experiment it is sitting in) is src, and "theirs" (the agent branch
// it is landing on) is dst. Git's own convention for a merge is the opposite —
// "ours" is what you are merging into — which is exactly why the store below
// this line speaks only of src and dst and refuses to guess.
//
// An unusable entry is an ERROR, never a skipped path: silently dropping one
// would refuse the commit for a path the caller believes it resolved.
func parseResolutions(raw any) (map[string]store.Resolution, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("`resolutions` must be an object keyed by fact path, got %T", raw)
	}
	out := make(map[string]store.Resolution, len(m))
	for file, v := range m {
		switch val := v.(type) {
		case string:
			switch val {
			case "ours":
				out[file] = store.Resolution{Side: store.ResolveSrc}
			case "theirs":
				out[file] = store.Resolution{Side: store.ResolveDst}
			default:
				return nil, fmt.Errorf("resolution for %q must be \"ours\", \"theirs\", or {\"body\": \"...\"}, got %q", file, val)
			}
		case map[string]any:
			body, ok := val["body"].(string)
			if !ok {
				return nil, fmt.Errorf("resolution object for %q needs a string `body`", file)
			}
			// An empty body is a real instruction — "this fact becomes
			// empty" — but it is almost never what anyone means, and a fact
			// file with no frontmatter fails validation later with a message
			// that does not mention resolutions. Refused here instead.
			if body == "" {
				return nil, fmt.Errorf("resolution body for %q is empty; to drop the fact, retract it instead", file)
			}
			out[file] = store.Resolution{Body: []byte(body)}
		default:
			return nil, fmt.Errorf("resolution for %q must be \"ours\", \"theirs\", or {\"body\": \"...\"}, got %T", file, v)
		}
	}
	return out, nil
}

// conflictReadingGuide names, per conflicting path, the commits to read it at.
//
// A path both sides ADDED has no version at the merge base, and that is said
// rather than glossed: knomit_explain for a path absent at a commit does not
// fail, it falls back to the nearest earlier version and returns a DIFFERENT
// fact with no indication. An agent told to read "the base version" of a dual
// add would be shown something unrelated and take it for the original.
//
// The hashes are FULL and never abbreviated: they are passed straight back as
// knomit_explain's `commit`, and a shortened one may not resolve.
func conflictReadingGuide(c *store.MergeConflictError) string {
	var b strings.Builder
	b.WriteString("Read each path with knomit_explain {file, commit}:\n")
	for _, p := range c.Paths {
		if c.HasBase(p) {
			fmt.Fprintf(&b, "  %s — base %s | %s %s | %s %s\n",
				p, c.BaseCommit, c.Src, c.SrcCommit, c.Dst, c.DstCommit)
			continue
		}
		fmt.Fprintf(&b, "  %s — NO BASE VERSION (both sides added it) | %s %s | %s %s\n",
			p, c.Src, c.SrcCommit, c.Dst, c.DstCommit)
	}
	return strings.TrimRight(b.String(), "\n")
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
			adjudications, perr := parseResolutions(req.GetArguments()["resolutions"])
			if perr != nil {
				return mcpgo.NewToolResultError(perr.Error()), nil
			}
			// A {body} resolution is a fact WRITE and is judged like one —
			// through the SAME function the REST endpoint calls, so the two
			// doors onto this action cannot disagree about what is acceptable.
			// The bytes that land are what it returns, never the caller's raw
			// body.
			expName := pickExperiment(name, active)
			adjudications, nerr := resolutions.Normalize(ctx, b.Write(),
				b.Write().AgentBranch(), store.ExperimentBranch(expName), adjudications)
			if nerr != nil {
				return mcpgo.NewToolResultError(nerr.Error()), nil
			}
			res, rerr = experimentCommit(ctx, mgr, svc, b, expName, adjudications)
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

	// WHERE THE CALLER IS NAMED decides how it gets moved. A handle names one
	// logical caller on a shared connection; a URL-scoped mount names none, so
	// there the session id plus the mount is the only pair available — and it
	// is usable precisely because the URL has already fixed the repo, so the
	// state can only choose a BRANCH within it. Migration 000008 carries that
	// argument in full.
	handle := bindingHandleFromContext(ctx)
	if handle == "" {
		sessionID, mountUID, plainMount := repos.MountSessionFromContext(ctx)

		// NOT a plain-branch mount: the URL names the experiment it serves.
		// Refused rather than silently ignored, because a caller that believed
		// it had switched would attribute its next write to the wrong one.
		//
		// Decided by the MARKER, never by the binding's current branch: a
		// session already inside an experiment on a plain mount has a binding
		// whose write branch is an experiment, and reading that would refuse
		// the perfectly ordinary act of opening a second one.
		if !plainMount {
			if store.ExperimentBranch(exp.Name) == b.WriteBranch() {
				res.Summary = fmt.Sprintf(
					"experiment %q is open on branch %q, which is the experiment this endpoint already addresses — nothing changed.",
					exp.Name, exp.Branch())
				return res, nil
			}
			return experimentResult{}, fmt.Errorf(
				"experiment %q was created, but this endpoint addresses %q by URL and cannot be switched: "+
					"connect to the mount for %q, or use a mount that names a plain branch",
				exp.Name, b.WriteBranch(), exp.Branch())
		}
		if sessionID == "" {
			// FAIL CLOSED. Keying on an empty session id would put every
			// anonymous caller of this mount into one shared experiment.
			return experimentResult{}, fmt.Errorf(
				"experiment %q was created, but this connection sent no MCP session id, so there is nothing to attach it to. "+
					"Reconnect with a session, or bind on the unscoped mount",
				exp.Name)
		}
		if err := setMountExperiment(ctx, mgr, sessionID, mountUID, exp.Name); err != nil {
			return experimentResult{}, err
		}
		res.Summary = fmt.Sprintf(
			"experiment %q is open on branch %q and THIS SESSION IS NOW INSIDE IT: every call on this connection now writes to %q, "+
				"until you commit or roll back. No reconnect needed.",
			exp.Name, exp.Branch(), exp.Branch())
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
func experimentCommit(ctx context.Context, mgr *repos.Manager, svc *store.Service, b *repos.Binding, name string, resolutions map[string]store.Resolution) (experimentResult, error) {
	if name == "" {
		return experimentResult{}, errors.New(
			"knomit_experiment commit needs a `name`, or a session that is inside an experiment")
	}
	result, err := svc.Experiments().CommitExperiment(ctx, name, resolutions)
	if err != nil {
		// A refused commit is a STATE, not a malfunction, so it comes back as
		// the paths plus what to do about them rather than as a bare error.
		//
		// It names THE THREE COMMITS per path, because resolving means reading
		// the fact at each of them: the fork point, what this experiment made
		// of it, and what the parent made of it. Telling an agent only that a
		// path conflicted leaves it to guess at a merge it cannot see.
		var conflict *store.MergeConflictError
		if errors.As(err, &conflict) {
			return experimentResult{}, fmt.Errorf(
				"commit refused: %s and %s both changed %d path(s) since the fork, and nothing was changed.\n"+
					"%s\n"+
					"Then retry with knomit_experiment {action: \"commit\", resolutions: {\"<path>\": \"ours\" | \"theirs\" | {\"body\": \"<merged text>\"}}} "+
					"— one entry per path listed above, and no entry for anything else. "+
					"\"ours\" keeps %s's version, \"theirs\" takes %s's. "+
					"Or rollback to discard the experiment",
				conflict.Src, conflict.Dst, len(conflict.Paths),
				conflictReadingGuide(conflict),
				conflict.Src, conflict.Dst)
		}
		return experimentResult{}, err
	}
	// Eager clear for the caller's own handle, so its very next call is back
	// on the agent branch without a round trip. Every OTHER handle pointing at
	// this experiment heals on its next resolution instead — see
	// repos.ResolveSessionBindingOnExperiment.
	clearHandleExperiment(ctx, mgr, bindingHandleFromContext(ctx))
	clearMountExperiment(ctx, mgr)
	agent := b.Write().AgentBranch()
	// A commit whose every conflict was resolved to the PARENT's side produces
	// a tree identical to the parent's: no merge commit is written and the
	// branch does not move. The experiment is still deleted, so the action did
	// happen — but saying "merged into" would credit it with changes it did
	// not make, and someone would go looking for a commit that never existed.
	summary := fmt.Sprintf("experiment %q merged into %q (%s) and deleted; this session is back on %q",
		name, agent, result.Mode, agent)
	if result.Mode == store.ModeNoop {
		summary = fmt.Sprintf(
			"experiment %q added nothing to %q — the merged result was identical to it, so no commit was written and %q did not move. The experiment is deleted and this session is back on %q",
			name, agent, agent, agent)
	}
	return experimentResult{
		Branch:  agent,
		Summary: summary,
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
	clearMountExperiment(ctx, mgr)
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

// setMountExperiment moves a URL-scoped session into an experiment. The mount
// twin of setHandleExperiment, and it fails the same way: the experiment
// exists either way, so a failure here must be REPORTED rather than swallowed —
// a caller told it is inside would write its next fact to the branch the URL
// names.
func setMountExperiment(ctx context.Context, mgr *repos.Manager, sessionID, mountUID, name string) error {
	if mgr == nil {
		return errStoreUnavailable
	}
	st := mgr.ClientSessions()
	if st == nil {
		return errors.New("session store unavailable — the experiment exists, but this session could not be moved into it")
	}
	if err := st.SetMountExperiment(ctx, sessionID, mountUID, name, time.Now()); err != nil {
		log.Warn().Err(err).Str("experiment", name).
			Msg("knomit_experiment: recording the mount's experiment failed")
		return errors.New("the experiment exists, but this session could not be moved into it; ask the operator to check the server log")
	}
	return nil
}

// clearMountExperiment puts a URL-scoped session back on the branch its URL
// names. Best effort, like clearHandleExperiment: by the time it runs the
// experiment is already gone from the store, so a failed clear self-corrects at
// the next resolution, which finds nothing to apply.
func clearMountExperiment(ctx context.Context, mgr *repos.Manager) {
	sessionID, mountUID, ok := repos.MountSessionFromContext(ctx)
	if !ok || mgr == nil {
		return
	}
	st := mgr.ClientSessions()
	if st == nil {
		return
	}
	if err := st.ClearMountExperiment(ctx, sessionID, mountUID); err != nil {
		log.Warn().Err(err).Msg("knomit_experiment: clearing the mount's experiment failed; it heals at the next resolution")
	}
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
