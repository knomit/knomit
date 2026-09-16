package mcp

import (
	"context"
	"encoding/json"
	"sort"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/federate"
	"knomit/internal/repos"
)

// catalogTool defines knomit_catalog: what this server serves, independent of
// what this session is bound to.
func catalogTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_catalog",
		mcpgo.WithDescription("List every knowledge base this server serves: repos (name, id, mode, branches, ontology root, profile; unavailable ones with the reason) and lenses (name, write repo, mounts), plus which one this session is bound to. Needs no binding — call it first on the unscoped endpoint to learn the names knomit_bind accepts."),
	)
}

// catalogRepo is one repo row. A repo is either live (mode and branches set) or
// unavailable (status/reason/detail set) — never both.
type catalogRepo struct {
	Name string `json:"name"`
	// ID is the 12-hex root-commit addressing id used by kb://<id>/ paths,
	// NEVER the registry uid: the uid is membership bookkeeping and is not a
	// name anyone types, while knomit_bind takes the name.
	//
	// omitempty for the same reason describeWriteDestination omits RepoID: the
	// id only exists once the store has opened, so federate.ID12(ri.ID()) is ""
	// for a repo that never did. An absent key is honest; `"id": ""` is a field
	// the caller would read as an id, and every happy-path test would still
	// pass. An unavailable repo therefore carries no id at all — and never the
	// registry uid as a stand-in.
	ID           string `json:"id,omitempty"`
	Mode         string `json:"mode,omitempty"`
	AgentBranch  string `json:"agent_branch,omitempty"`
	ReadBranch   string `json:"read_branch,omitempty"`
	OntologyRoot string `json:"ontology_root,omitempty"`
	Profile      string `json:"profile,omitempty"`

	Status string `json:"status,omitempty"` // "unavailable"
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// catalogMount is one lens member, by repo NAME at the branch it is read at.
type catalogMount struct {
	Repo        string `json:"repo"`
	Branch      string `json:"branch,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
}

type catalogLens struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Write       string         `json:"write,omitempty"`
	Mounts      []catalogMount `json:"mounts"`
}

// catalogBound is where this session currently points, omitted when unbound.
type catalogBound struct {
	Kind string `json:"kind"` // repo | lens
	Name string `json:"name"`
}

type catalogResponse struct {
	Bound  *catalogBound `json:"bound,omitempty"`
	Repos  []catalogRepo `json:"repos"`
	Lenses []catalogLens `json:"lenses"`
}

// CatalogHandler returns the handler for knomit_catalog.
//
// It requires NO binding — that is the point: on the unscoped mount a fresh
// session has none, and knomit_repos can only describe the binding it does not
// yet have, so without this an agent cannot discover the names knomit_bind
// accepts.
//
// It is a CHEAP INDEX, and the claim is precise: no README, no LICENSE, no
// per-fact read. Values come from control-plane state (Manager.ForEach,
// Manager.Unavailable, LensRegistry.List, and the registry row for the profile)
// plus each instance's cached identity. It is NOT literally store-free —
// ri.ID() falls back to a WithRead → RootCommit on a cache miss — but that is
// exactly what the REST repo list does for ShortID, so this is parity with the
// established rule rather than an exception to it. The bound knowledge base's
// full instructions arrive in the knomit_bind result instead.
func CatalogHandler(mgr *repos.Manager) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, catalogTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if mgr == nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}

		lenses := catalogLenses(mgr)
		if lenses == nil {
			return mcpgo.NewToolResultError(
				"the lens registry is unavailable, so this listing would wrongly show no lenses — retry shortly"), nil
		}
		resp := catalogResponse{
			Bound:  boundOf(ctx),
			Repos:  catalogRepos(mgr),
			Lenses: lenses,
		}
		out, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultError("marshal error: " + err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}

// boundOf reports the session's current binding as kind+name, or nil when
// unbound. On a URL-scoped mount this is whatever the URL named.
func boundOf(ctx context.Context) *catalogBound {
	pin := repos.BindingPinFromContext(ctx)
	if pin == "" {
		return nil
	}
	kind, _, err := repos.ParsePin(pin)
	if err != nil {
		return nil
	}
	b, ok := repos.BindingFromContextOpt(ctx)
	if !ok {
		ri, riOK := repos.RepoFromContextOpt(ctx)
		if !riOK {
			return nil
		}
		return &catalogBound{Kind: kind, Name: ri.Name()}
	}
	return &catalogBound{Kind: kind, Name: b.Name()}
}

// catalogRepos lists live repos and then the registered-but-unopenable ones,
// all sorted by name. A broken repo stays visible so the agent learns WHY a
// bind would fail rather than finding the name simply absent.
//
// NOTHING that touches mgr may run inside the ForEach callback. ForEach holds
// m.mu.RLock for the whole iteration, and sync.RWMutex is not reentrant: a
// second m.mu.RLock (profileFor → mgr.Repos(), or mgr.RepoLabel) deadlocks the
// entire Manager the moment a writer is pending, because Go blocks new readers
// once a writer queues and the outer reader cannot release while it is stuck in
// this callback. An unauthenticated, binding-free tool call would wedge the
// server. internal/repos/manager.go states the same rule for validateLensLocked.
//
// So the callback only SNAPSHOTS (name, instance) pairs and every per-repo
// lookup happens after ForEach returns — the shape handleHALRepos already uses.
// It also keeps the per-repo control.db queries out from under the read lock.
func catalogRepos(mgr *repos.Manager) []catalogRepo {
	names := make([]string, 0)
	instances := make(map[string]*repos.RepoInstance)
	mgr.ForEach(func(name string, ri *repos.RepoInstance) {
		// Manager.Set stores a nil instance rather than deleting the key, and
		// ForEach does not filter, so the map can legally yield one. No
		// production path does that today, but an unauthenticated tool must not
		// be one nil away from a panic.
		if ri == nil {
			return
		}
		names = append(names, name)
		instances[name] = ri
	})

	out := make([]catalogRepo, 0, len(names))
	for _, name := range names {
		ri := instances[name]
		out = append(out, catalogRepo{
			Name:         name,
			ID:           federate.ID12(ri.ID()),
			Mode:         repoMode(ri),
			AgentBranch:  ri.AgentBranch(),
			ReadBranch:   ri.ReadBranch(),
			OntologyRoot: ri.OntologyRoot(),
			Profile:      profileFor(mgr, ri),
		})
	}
	for _, u := range mgr.Unavailable() {
		out = append(out, catalogRepo{
			Name:   u.Record.Name,
			Status: "unavailable",
			Reason: u.Reason,
			Detail: u.Detail,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// repoMode names what a binding to this repo could do. "read-only" is the
// ontology-less case: writable only if the repo's own agent branch is one it
// may author on.
func repoMode(ri *repos.RepoInstance) string {
	switch {
	case ri.Subscribed():
		return "subscribe"
	case ri.WritableBranch(ri.AgentBranch()):
		return "writable"
	default:
		return "read-only"
	}
}

// catalogLenses lists lenses by name, resolving each member uid to its repo
// name. An empty stored branch means "that member's read branch at resolve
// time", so it is shown resolved rather than blank — the same rule
// NewBindingOfLens applies, including ordering mounts by repo name.
func catalogLenses(mgr *repos.Manager) []catalogLens {
	out := []catalogLens{}
	reg := mgr.LensRegistry()
	if reg == nil {
		// Same conflation as a failed List: "the registry never started" must
		// not reach the agent as "this server has no lenses".
		log.Warn().Msg("knomit_catalog: lens registry not started")
		return nil
	}
	lenses, err := reg.List()
	if err != nil {
		// Never render a failed query as an empty list: the agent cannot tell
		// "this server has no lenses" from "the lookup broke", and would bind
		// to a bare repo instead of the lens it needed with nothing recording
		// why. Surfaced as an error by the caller.
		log.Warn().Err(err).Msg("knomit_catalog: lens registry list failed")
		return nil
	}
	for _, l := range lenses {
		cl := catalogLens{Name: l.Name, Description: l.Description, Mounts: []catalogMount{}}
		if w := mgr.GetByUID(l.WriteUID); w != nil {
			cl.Write = w.Name()
		} else {
			cl.Write = mgr.RepoLabel(l.WriteUID)
		}
		for _, lr := range l.Reads {
			ri := mgr.GetByUID(lr.RepoUID)
			if ri == nil {
				cl.Mounts = append(cl.Mounts, catalogMount{
					Repo: mgr.RepoLabel(lr.RepoUID), Unavailable: true,
				})
				continue
			}
			branch := lr.Branch
			if branch == "" {
				branch = ri.ReadBranch()
			}
			cl.Mounts = append(cl.Mounts, catalogMount{Repo: ri.Name(), Branch: branch})
		}
		sort.Slice(cl.Mounts, func(i, j int) bool { return cl.Mounts[i].Repo < cl.Mounts[j].Repo })
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
