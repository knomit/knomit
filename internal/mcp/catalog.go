package mcp

import (
	"context"
	"encoding/json"
	"sort"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

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
// It is a CHEAP INDEX. Every value comes from control-plane state already in
// memory (Manager.ForEach, Manager.Unavailable, LensRegistry.List, and the
// registry row for the profile); it opens no repo store and reads no README or
// LICENSE, exactly as the REST repo list does. The bound knowledge base's full
// instructions arrive in the knomit_bind result instead.
func CatalogHandler(mgr *repos.Manager) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, catalogTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if mgr == nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}

		resp := catalogResponse{
			Bound:  boundOf(ctx),
			Repos:  catalogRepos(mgr),
			Lenses: catalogLenses(mgr),
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
func catalogRepos(mgr *repos.Manager) []catalogRepo {
	out := []catalogRepo{}
	mgr.ForEach(func(name string, ri *repos.RepoInstance) {
		out = append(out, catalogRepo{
			Name:         name,
			ID:           federate.ID12(ri.ID()),
			Mode:         repoMode(ri),
			AgentBranch:  ri.AgentBranch(),
			ReadBranch:   ri.ReadBranch(),
			OntologyRoot: ri.OntologyRoot(),
			Profile:      profileFor(mgr, ri),
		})
	})
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
		return out
	}
	lenses, err := reg.List()
	if err != nil {
		return out
	}
	for _, l := range lenses {
		cl := catalogLens{Name: l.Name, Description: l.Description, Mounts: []catalogMount{}}
		if w := mgr.GetByUID(l.WriteUID); w != nil {
			cl.Write = w.Name()
		} else {
			cl.Write = repoNameByUID(mgr, l.WriteUID)
		}
		for _, lr := range l.Reads {
			ri := mgr.GetByUID(lr.RepoUID)
			if ri == nil {
				cl.Mounts = append(cl.Mounts, catalogMount{
					Repo: repoNameByUID(mgr, lr.RepoUID), Unavailable: true,
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

// repoNameByUID resolves a member uid to its repo NAME for display when no
// live instance exists, falling back to the uid only when the registry does not
// know it either. A member that failed to open still has a registry row, so the
// name is almost always available — and a bare uid names nothing a reader has
// been shown.
//
// This mirrors Manager.repoLabel, which internal/mcp cannot call: it is
// unexported and lives beside the binding code.
func repoNameByUID(mgr *repos.Manager, uid string) string {
	reg := mgr.Repos()
	if reg == nil || uid == "" {
		return uid
	}
	if rec, ok, err := reg.Get(uid); err == nil && ok && rec.Name != "" {
		return rec.Name
	}
	return uid
}
