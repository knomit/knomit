package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/federate"
	"knomit/internal/repos"
)

// reposTool defines knomit_repos: the ONE discovery tool.
//
// It answers both "what does this server serve" and "what is behind my
// binding", because the second is a nested section of the first rather than a
// second tool — which is why the separately built knomit_catalog was folded in
// here before it ever shipped.
//
// The shape is fixed PER STATE, not per session, which is what keeps one name
// from meaning two things: `repos` is always present, `bound` appears only when
// the session points at something, and `lenses` is replaced by `lenses_error`
// when the registry lookup fails. Each key's presence reports a fact about the
// world rather than a mode the tool is in.
func reposTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_repos",
		mcpgo.WithDescription("List every repo and lens this server serves, and what this session is bound to. Needs no binding — call it first on the unscoped endpoint to learn the names knomit_bind accepts. Use a repo's id to interpret kb://<id>/… paths. Note a mount's source slug is NOT what a src:// ref carries — src:// refs are keyed by the SOURCE repo's own root commit, obtained by running git in that checkout."),
	)
}

// reposMount is one mount row of the CURRENT binding, inside `bound`.
type reposMount struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Branch string `json:"branch"`
	Role   string `json:"role"`
	Source string `json:"source,omitempty"`
	// WriteBranch is set only on the read+write row. Writes always commit to
	// the write repo's agent branch (RFC decision 19 / gotcha M-4), which may
	// differ from Branch — the branch the write repo is READ at through a lens.
	WriteBranch string `json:"write_branch,omitempty"`
	// Mode is "subscribe" for a read-only follower of a remote branch; absent
	// otherwise.
	Mode string `json:"mode,omitempty"`
}

// boundSection is the `bound` key: what THIS session points at, present only
// when it points at anything.
//
// It carries the mount table knomit_repos returned before the catalogue was
// folded in. A session whose STORED pin no longer resolves is a third state,
// distinct from both bound and never-bound: Status "unresolvable" with the
// reason, since every other tool is already telling that agent to bind again
// and this is the tool it calls to find out why. Every field is omitempty, so
// no partial state serializes a "" key.
type boundSection struct {
	Binding string       `json:"binding,omitempty"`
	Mounts  []reposMount `json:"mounts,omitempty"`

	Kind   string `json:"kind,omitempty"` // repo | lens, from the dead pin's prefix
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"` // "unresolvable"
	Error  string `json:"error,omitempty"`
}

// reposResponse is the knomit_repos envelope.
//
// Lenses is a POINTER so the three states stay distinct on the wire: a real
// list, an empty list (`"lenses": []` — this server has none), and a failed
// lookup, which omits the key entirely and sets LensesError. A plain slice
// with omitempty could not tell the last two apart, since Go omits an empty
// slice and an agent reading `[]` would conclude no lens exists and bind to a
// bare repo instead of the lens it needed.
type reposResponse struct {
	Repos       []reposRepo   `json:"repos"`
	Lenses      *[]reposLens  `json:"lenses,omitempty"`
	LensesError string        `json:"lenses_error,omitempty"`
	Bound       *boundSection `json:"bound,omitempty"`
}

// ReposHandler returns the handler for knomit_repos.
//
// It requires NO binding: on the unscoped mount a fresh session has none, and
// this is how an agent learns the names knomit_bind accepts.
//
// It is a CHEAP INDEX, and the claim is precise: no README, no LICENSE, no
// per-fact read. Values come from control-plane state (Manager.ForEach,
// Manager.Unavailable, LensRegistry.List, and the registry row for the profile)
// plus each instance's cached identity. It is NOT literally store-free —
// ri.ID() falls back to a WithRead → RootCommit on a cache miss — but that is
// exactly what the REST repo list does for ShortID, so this is parity with the
// established rule rather than an exception to it.
//
// There is deliberately no paging and no filter argument: ~150 bytes a row puts
// 50 repos and 20 lenses around 12 KB, smaller than a query page, and paging
// would add cursor state to a list the agent scans in one go. The growth path,
// if it is ever needed, is a `match` substring argument.
func ReposHandler(mgr *repos.Manager) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, reposTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if mgr == nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}
		// A lens lookup failure DEGRADES rather than aborting. Before the fold
		// this call could not fail at all, so failing the whole tool would cost
		// a bound agent its own mount table over an unrelated control-plane
		// error. Omitting the key and naming the failure keeps both guarantees:
		// the agent never reads "no lenses" from a broken lookup, and it still
		// gets everything that did resolve.
		resp := reposResponse{
			Repos: listRepos(mgr),
			Bound: boundOf(ctx, mgr),
		}
		if lenses := listLenses(mgr); lenses != nil {
			resp.Lenses = &lenses
		} else {
			resp.LensesError = "the lens registry is unavailable, so lenses could not be listed — retry shortly; the repos above are unaffected"
		}
		out, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultError("marshal error: " + err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}

// boundFor builds the `bound` section for a live binding.
//
// knomit_bind returns this object ALONE — not the whole catalogue — for the
// binding it just made, so the agent does not have to call knomit_repos to
// learn what it just bound. Both tools share this builder so the mount table
// cannot drift between them.
func boundFor(b *repos.Binding) *boundSection {
	out := &boundSection{Binding: b.Name(), Mounts: []reposMount{}}
	for _, rt := range b.Reads() {
		role := "read"
		var writeBranch string
		if rt.RI == b.Write() && b.WriteOK() {
			role = "read+write"
			// Writes commit here, not to rt.Branch (RFC decision 19 / M-4).
			writeBranch = b.Write().AgentBranch()
		}
		mode := ""
		if rt.RI.Subscribed() {
			mode = "subscribe"
		}
		out.Mounts = append(out.Mounts, reposMount{
			Name:        rt.RI.Name(),
			ID:          federate.ID12(rt.RI.ID()),
			Branch:      rt.Branch,
			Role:        role,
			Source:      rt.Source,
			WriteBranch: writeBranch,
			Mode:        mode,
		})
	}
	return out
}

// boundOf reports what this session points at: the live binding's mount table,
// the reason a stored pin will not resolve, or nothing at all.
func boundOf(ctx context.Context, mgr *repos.Manager) *boundSection {
	if b, ok := repos.BindingFromContextOpt(ctx); ok {
		return boundFor(b)
	}
	if ri, ok := repos.RepoFromContextOpt(ctx); ok {
		// A URL-scoped mount carries only a RepoInstance; synthesize the
		// lens-of-one exactly as the tool handlers do.
		branch, _ := repos.BranchFromContextOpt(ctx)
		return boundFor(repos.NewBindingOfRepo(ri, branch))
	}
	if err, ok := repos.BindingErrorFromContext(ctx); ok {
		return unresolvableBound(mgr, err)
	}
	return nil
}

// reposRepo is one repo row. A repo is either live (mode and branches set) or
// unavailable (status/reason/detail set) — never both.
type reposRepo struct {
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

// reposLensMount is one lens member, by repo NAME at the branch it is read at.
type reposLensMount struct {
	Repo        string `json:"repo"`
	Branch      string `json:"branch,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
}

type reposLens struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Write       string           `json:"write,omitempty"`
	Mounts      []reposLensMount `json:"mounts"`
}

// listRepos lists live repos and then the registered-but-unopenable ones,
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
func listRepos(mgr *repos.Manager) []reposRepo {
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

	out := make([]reposRepo, 0, len(names))
	for _, name := range names {
		ri := instances[name]
		out = append(out, reposRepo{
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
		out = append(out, reposRepo{
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

// listLenses lists lenses by name, resolving each member uid to its repo
// name. An empty stored branch means "that member's read branch at resolve
// time", so it is shown resolved rather than blank — the same rule
// NewBindingOfLens applies, including ordering mounts by repo name.
func listLenses(mgr *repos.Manager) []reposLens {
	out := []reposLens{}
	reg := mgr.LensRegistry()
	if reg == nil {
		// Same conflation as a failed List: "the registry never started" must
		// not reach the agent as "this server has no lenses".
		log.Warn().Msg("knomit_repos: lens registry not started")
		return nil
	}
	lenses, err := reg.List()
	if err != nil {
		// Never render a failed query as an empty list: the agent cannot tell
		// "this server has no lenses" from "the lookup broke", and would bind
		// to a bare repo instead of the lens it needed with nothing recording
		// why. Surfaced as an error by the caller.
		log.Warn().Err(err).Msg("knomit_repos: lens registry list failed")
		return nil
	}
	for _, l := range lenses {
		cl := reposLens{Name: l.Name, Description: l.Description, Mounts: []reposLensMount{}}
		if w := mgr.GetByUID(l.WriteUID); w != nil {
			cl.Write = w.Name()
		} else {
			cl.Write = mgr.RepoLabel(l.WriteUID)
		}
		for _, lr := range l.Reads {
			ri := mgr.GetByUID(lr.RepoUID)
			if ri == nil {
				cl.Mounts = append(cl.Mounts, reposLensMount{
					Repo: mgr.RepoLabel(lr.RepoUID), Unavailable: true,
				})
				continue
			}
			branch := lr.Branch
			if branch == "" {
				branch = ri.ReadBranch()
			}
			cl.Mounts = append(cl.Mounts, reposLensMount{Repo: ri.Name(), Branch: branch})
		}
		sort.Slice(cl.Mounts, func(i, j int) bool { return cl.Mounts[i].Repo < cl.Mounts[j].Repo })
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// unresolvableBound describes a stored binding that no longer resolves.
//
// The kind comes from the pin the resolver failed on, which *SessionBindingError
// carries for exactly this purpose — the context holds only the error, so
// without that field neither kind nor name would be recoverable here.
//
// The NAME is set only when the registry still has a real one. A dead pin's uid
// must never be shown: it is not a name anyone types and knomit_bind would not
// accept it, so an absent name is the honest answer.
func unresolvableBound(mgr *repos.Manager, err error) *boundSection {
	out := &boundSection{Status: "unresolvable", Error: err.Error()}

	var sbe *repos.SessionBindingError
	if !errors.As(err, &sbe) || sbe.Pin == "" {
		return out // e.g. the store-lookup failure, which is about no pin
	}
	kind, uid, perr := repos.ParsePin(sbe.Pin)
	if perr != nil {
		return out // a malformed pin names no kind
	}
	out.Kind = kind

	switch kind {
	case "repo":
		if reg := mgr.Repos(); reg != nil {
			if rec, ok, gerr := reg.Get(uid); gerr == nil && ok && rec.Name != "" {
				out.Name = rec.Name
			}
		}
	case "lens":
		if reg := mgr.LensRegistry(); reg != nil {
			if l, ok, gerr := reg.GetByUID(uid); gerr == nil && ok && l.Name != "" {
				out.Name = l.Name
			}
		}
	}
	return out
}
