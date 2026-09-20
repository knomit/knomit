// Binding is the runtime object every MCP handler receives instead of a bare
// *RepoInstance (lenses RFC §5.1). It carries the single write target and the
// federated read set (each mount at its pinned branch). Phase 2 keeps reads
// single-target (handlers use Write() only); federation consumes Reads() in
// Phase 3.
package repos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/store"
)

// ReadTarget is one read mount of a binding: a repo at a pinned branch, with
// its optional src:// source slug for verify metadata.
type ReadTarget struct {
	RI     *RepoInstance
	Branch string
	Source string
}

// Binding binds one write repo and N read mounts under a name.
type Binding struct {
	write   *RepoInstance
	writeOK bool
	// writeBranch OVERRIDES the default write target, and is non-empty ONLY
	// when the write repo is pinned at an experiment it may write. Empty
	// means "the write repo's agent branch", which is what every binding
	// meant before experiments existed — so an absent override reproduces the
	// old behaviour exactly rather than approximating it.
	writeBranch string
	name        string
	// pinID is the binding's STABLE identity — see PinID for the contract.
	pinID string
	reads []ReadTarget
}

// Write returns the single write repo.
func (b *Binding) Write() *RepoInstance { return b.write }

// WriteOK reports whether write tools may operate on this binding. A
// lens-of-one bound to a non-writable branch is a read-only view. Where an
// allowed write LANDS is WriteBranch's question, not this one.
func (b *Binding) WriteOK() bool { return b.writeOK }

// WriteBranch is the branch a write through this binding commits to: the
// experiment the write repo is pinned at, or — the ordinary case, and every
// case before experiments — that repo's own agent branch.
//
// It returns "" on a read-only view, deliberately. A caller that skipped the
// WriteOK gate then gets a value that cannot be mistaken for a destination,
// instead of a plausible branch name it would happily commit to.
func (b *Binding) WriteBranch() string {
	if !b.writeOK {
		return ""
	}
	if b.writeBranch != "" {
		return b.writeBranch
	}
	return b.write.AgentBranch()
}

// experimentPin returns branch when it is an experiment ri may write, and ""
// otherwise. The two binding constructors share it so "pinned at a writable
// experiment" has ONE definition; a second copy would be free to disagree
// about, say, whether an unrecorded exp/* ref counts.
func experimentPin(ri *RepoInstance, branch string) string {
	if store.IsExperimentBranch(branch) && ri.WritableBranch(branch) {
		return branch
	}
	return ""
}

// WriteMountBranch returns the branch the write repo is READ at in this
// binding (its read mount's pinned branch). It is a READ question and stays
// one: the read-only-view error names it, cursor sessions pin it, and the
// write repo's read fan-out searches it. Where a write LANDS is
// WriteBranch — the two agree when the pin is an experiment and differ
// whenever it is anything else the repo cannot write.
//
// The AgentBranch fallback is for a binding with no read mount for its own
// write repo, which production never builds; NewBindingForTest can.
func (b *Binding) WriteMountBranch() string {
	for _, rt := range b.reads {
		if rt.RI == b.write {
			return rt.Branch
		}
	}
	return b.write.AgentBranch()
}

// Name returns the lens name, or the repo name for a lens-of-one. Human-
// readable; used in error messages and logs. Can change under a rename — use
// PinID, never Name, for any comparison that must survive one.
func (b *Binding) Name() string { return b.name }

// PinID returns the binding's STABLE identity, used to pin MCP tool-session
// cursors (lenses RFC §7.3): repo:<uid> for a lens-of-one, lens:<uid> for a
// lens. Distinct from Name, which is for humans and can change — use PinID,
// never Name, for any comparison that must survive a rename.
//
// Prefixed deliberately. Once one side stops being a name the two value
// spaces are no longer self-evidently disjoint, and a legal repo name can
// parse as a ksuid (see the migrate-registry ksuid-shaped-name gotcha). The
// prefix removes the question instead of arguing about probabilities.
func (b *Binding) PinID() string { return b.pinID }

// pinOf builds a PinID value, failing CLOSED on an empty uid: it returns ""
// rather than a bare "repo:"/"lens:" prefix. Four independent barriers make
// an empty uid unreachable today (Registry.Insert rejects it; migrate-registry
// mints one; every live instance's uid comes from a registry row; a corrupted
// row can't open, so it never reaches a constructor) — but every other
// empty-uid site in this package guards explicitly (registry.go, swapstore.go,
// manager.go), and this is the one that guards a security check, so it does
// too. "" is deliberately not a valid PinID: the resume comparisons
// (query.go, explain.go) additionally reject a stored/computed "" outright,
// so a uid-less binding can mint a session but can never resume one — a
// bare-prefix collision between two uid-less bindings never gets the chance
// to matter.
func pinOf(prefix, uid string) string {
	if uid == "" {
		return ""
	}
	return prefix + uid
}

// IsLens reports whether this binding federates across more than its own
// write repo — i.e. at least one read mount is a different repo. A lens-of-one
// (the /repos/{repo}/… path) is not a lens: its only read mount is the write
// repo itself, so session instructions stay single-repo (RFC §9.4).
func (b *Binding) IsLens() bool {
	for _, rt := range b.reads {
		if rt.RI != b.write {
			return true
		}
	}
	return false
}

// FromLens reports whether this binding was RESOLVED FROM a persisted lens
// definition, rather than synthesized for a single repo (the /repos/{repo}/…
// path). Read off PinID, which is prefixed exactly so the two provenances are
// distinguishable without a name comparison.
//
// Distinct from IsLens, and the difference is load-bearing: IsLens asks about
// federation BREADTH — does this binding read more than its own write repo —
// and a lens that mounts a single member federates across nothing, so IsLens
// says false about a binding that unambiguously came from a lens. Anything
// reporting HOW THE CALLER CONNECTED wants FromLens; anything deciding whether
// federation machinery has more than one mount to fan out over wants IsLens.
//
// A uid-less binding mints an empty PinID (pinOf fails closed) and therefore
// reads as not-from-a-lens. That is the same four-barriers-unreachable case
// PinID documents, and the safe direction to fail: a missing lens name is a
// thinner report, not a wrong one.
func (b *Binding) FromLens() bool { return strings.HasPrefix(b.pinID, "lens:") }

// Reads returns the read mounts (the write repo is always among them).
func (b *Binding) Reads() []ReadTarget { return b.reads }

// ByID routes a repo ID — the full root-commit hash or its 12-hex wire
// prefix (RFC §6.1) — to its mount. Unambiguous within a valid binding:
// replica mounts are rejected at lens creation (decision 18).
func (b *Binding) ByID(id string) (ReadTarget, bool) {
	if id == "" {
		return ReadTarget{}, false
	}
	for _, rt := range b.reads {
		full := rt.RI.ID()
		if full == id || (len(id) == 12 && strings.HasPrefix(full, id)) {
			return rt, true
		}
	}
	return ReadTarget{}, false
}

// NewBindingOfRepo synthesizes the lens-of-one for a single repo bound to
// branch: write = repo, reads = [repo@branch], writable iff branch is one the
// repo may author on — its own agent branch, or an experiment forked from it.
func NewBindingOfRepo(ri *RepoInstance, branch string) *Binding {
	// An empty branch defaults to the repo's own READ branch — the agent
	// branch, or the upstream for a subscription, which has no agent branch to
	// fall back to.
	if branch == "" {
		branch = ri.ReadBranch()
	}
	// writeOK stays STRICTLY the bound branch's classification: a mount bound
	// to main is a read-only view, and always was. The only thing experiments
	// change here is that WritableBranch now says yes to one more class, and
	// when that class is what we are bound to, the write goes there rather
	// than to the agent branch.
	return &Binding{
		write:       ri,
		writeOK:     ri.WritableBranch(branch),
		writeBranch: experimentPin(ri, branch),
		name:        ri.Name(),
		pinID:       pinOf("repo:", ri.UID()),
		reads:       []ReadTarget{{RI: ri, Branch: branch}},
	}
}

// NewBindingForTest builds a binding directly from a write repo and explicit
// read mounts, bypassing lens resolution. Intended for federation/handler
// tests in sibling packages that need a multi-mount binding without standing
// up a full Manager — mirrors NewTestInstanceWithDeps. Production code must
// use NewBindingOfRepo or NewBindingOfLens.
func NewBindingForTest(write *RepoInstance, reads ...ReadTarget) *Binding {
	// writeOK stays unconditionally true — that is this constructor's whole
	// point, and tightening it would silently turn existing federation
	// fixtures into read-only views. The write BRANCH is still derived the
	// way NewBindingOfLens derives it, so a fixture that pins its write
	// member at an experiment gets a binding that writes there; otherwise it
	// is the agent branch, exactly as before.
	writeBranch := ""
	for _, rt := range reads {
		if rt.RI == write {
			writeBranch = experimentPin(write, rt.Branch)
			break
		}
	}
	return &Binding{
		write:       write,
		writeOK:     true,
		writeBranch: writeBranch,
		name:        write.Name(),
		pinID:       pinOf("repo:", write.UID()),
		reads:       reads,
	}
}

// NewBindingOfLens resolves a persisted lens definition against the manager's
// active repos. Members are named by registry uid, so resolution goes through
// m.GetByUID — a registered-but-unopened repo (missing/unopenable file) has a
// registry row and therefore a valid uid, but no live instance, and fails
// loudly here (RFC §9.1): a lens must never silently shrink its read set. Empty
// read branches default to each member's own agent branch at resolve time.
func NewBindingOfLens(m *Manager, l Lens) (*Binding, error) {
	write := m.GetByUID(l.WriteUID)
	if write == nil {
		return nil, fmt.Errorf("lens %q references unavailable repo %q", l.Name, m.RepoLabel(l.WriteUID))
	}
	reads := make([]ReadTarget, 0, len(l.Reads))
	for _, lr := range l.Reads {
		ri := m.GetByUID(lr.RepoUID)
		if ri == nil {
			return nil, fmt.Errorf("lens %q references unavailable repo %q", l.Name, m.RepoLabel(lr.RepoUID))
		}
		// Empty read pins default to each member's own READ branch at resolve
		// time — the agent branch, or the upstream for a subscription.
		branch := lr.Branch
		if branch == "" {
			branch = ri.ReadBranch()
		}
		reads = append(reads, ReadTarget{RI: ri, Branch: branch, Source: lr.Source})
	}
	// Mount order is by repo NAME, not by the uid the rows are stored under.
	// Reads() order is a real contract — it is the tie-break federation fuses on
	// (mount 0 wins an RRF tie) — and uids are ksuids, whose ordering within one
	// second is random. Sorting here keeps that contract stable and legible
	// while membership stays uid-keyed. Names are unique among ACTIVE repos and
	// every member here is active, so the order is total.
	sort.Slice(reads, func(i, j int) bool { return reads[i].RI.Name() < reads[j].RI.Name() })
	// A lens writes to the write member's OWN agent branch — which is
	// precisely why this asks the same question every other write path asks,
	// rather than asserting the answer. A repo whose ontology could not be
	// established accepts no writes through any door, and a lens that
	// hardcoded `true` was a door.
	//
	// The read pin does NOT override that rule in general, and must not start
	// to. A lens may legitimately pin its write member at the consensus
	// branch FOR READS while writing to that member's agent branch; deriving
	// writeOK from the pin unconditionally would turn every such lens
	// silently read-only. The ONE pin that redirects the write is an
	// experiment the member may write — the case experiments exist for.
	writeBranch := ""
	for _, rt := range reads {
		if rt.RI == write {
			writeBranch = experimentPin(write, rt.Branch)
			break
		}
	}
	return &Binding{
		write:       write,
		writeOK:     writeBranch != "" || write.WritableBranch(write.AgentBranch()),
		writeBranch: writeBranch,
		name:        l.Name,
		pinID:       pinOf("lens:", l.UID),
		reads:       reads,
	}, nil
}

// bindingCtxKey is the private context key for an explicit Binding.
type bindingCtxKey struct{}

// WithBinding stores an explicit binding in the context (LensMiddleware).
func WithBinding(ctx context.Context, b *Binding) context.Context {
	return context.WithValue(ctx, bindingCtxKey{}, b)
}

// BindingFromContextOpt retrieves an explicit binding if present.
func BindingFromContextOpt(ctx context.Context) (*Binding, bool) {
	b, ok := ctx.Value(bindingCtxKey{}).(*Binding)
	return b, ok
}

// BindingFromContext returns the request's binding. When no explicit binding
// was set (the /repos/{repo}/branches/{branch}/mcp path, and every direct
// handler call in tests), it synthesizes the lens-of-one from the context
// RepoInstance and bound branch — so the single-repo path needs no middleware
// change and stays behavior-identical. Panics when neither a binding nor a
// RepoInstance is in the context: that is a programming error, mirroring
// RepoFromContext. MCP tool handlers use RequireBinding, which never panics.
func BindingFromContext(ctx context.Context) *Binding {
	if b, ok := BindingFromContextOpt(ctx); ok {
		return b
	}
	ri, ok := RepoFromContextOpt(ctx)
	if !ok {
		panic("BindingFromContext: no Binding or RepoInstance in context")
	}
	branch, _ := BranchFromContextOpt(ctx)
	return NewBindingOfRepo(ri, branch)
}

// ErrUnbound is what every tool returns on a session-bound mount that has not
// chosen a repo or lens yet. The text is the tool error the agent reads, so it
// names the tool it must call.
var ErrUnbound = errors.New("no repo or lens is bound to this session — call knomit_bind with a repo or lens name first")

// RequireBinding is BindingFromContext for callers that must FAIL rather than
// panic: the MCP tool handlers, which on the unscoped mount can legitimately
// run with nothing bound.
//
// Order matters. An explicit Binding wins. Failing that, a RepoInstance in the
// context means a URL-scoped mount (or a direct handler call in a test), and
// the lens-of-one is synthesized exactly as BindingFromContext does. Only then
// do the session-bound cases apply: a stored pin that would not resolve
// surfaces its own reason, and anything else is simply unbound.
func RequireBinding(ctx context.Context) (*Binding, error) {
	if b, ok := BindingFromContextOpt(ctx); ok {
		return b, nil
	}
	if ri, ok := RepoFromContextOpt(ctx); ok {
		branch, _ := BranchFromContextOpt(ctx)
		return NewBindingOfRepo(ri, branch), nil
	}
	if err, ok := BindingErrorFromContext(ctx); ok {
		return nil, err
	}
	return nil, ErrUnbound
}

// ResolvedBindingFromContext returns what the tool gate resolved for this
// request — handle, pin and branch — or the zero value on a mount that has no
// recorder (every URL-scoped one) or a request that resolved nothing.
//
// The URL-scoped mounts deliberately yield an empty HANDLE with a non-empty
// pin from BindingPinFromContext: their target came from the path, so there is
// no handle to report, and the session binding set stays empty for them.
func ResolvedBindingFromContext(ctx context.Context) ResolvedBinding {
	if p, ok := PinRecorderFromContext(ctx); ok {
		if rb := p.Resolved(); rb.Pin != "" {
			return rb
		}
	}
	// No recorder, or nothing recorded: fall back to whatever the context can
	// say about the pin, so a URL-scoped request still stamps its row.
	return ResolvedBinding{Pin: BindingPinFromContext(ctx)}
}

// BindingPinFromContext resolves the request's binding to a PinID
// ("repo:<uid>" or "lens:<uid>"), or "" when the context carries neither a
// Binding nor a RepoInstance.
//
// It exists because the two MCP mounts populate the context DIFFERENTLY and
// neither plain accessor is safe on both: the lens mount sets an explicit
// Binding (WithBinding, in LensMiddleware) while the repo mount sets only a
// RepoInstance and a branch. So BindingFromContextOpt returns false on every
// repo-scoped request — the common path — and BindingFromContext, which
// synthesizes the lens-of-one from the RepoInstance, PANICS when the context
// has neither. Every caller that wants a pin from an arbitrary request needs
// this exact guarded order, so it lives here rather than being re-derived at
// each call site.
func BindingPinFromContext(ctx context.Context) string {
	if b, ok := BindingFromContextOpt(ctx); ok {
		return b.PinID()
	}
	if _, ok := RepoFromContextOpt(ctx); ok {
		return BindingFromContext(ctx).PinID()
	}
	// The unscoped mount is a THIRD shape: its binding is named by a handle
	// argument inside the request body, so it is not in the context at all when
	// the request arrives — the tool gate resolves it and records it here. See
	// PinRecorder for why the value has to travel this way.
	if p, ok := PinRecorderFromContext(ctx); ok {
		return p.Pin()
	}
	return ""
}

// RepoLabel resolves a registry uid to its display NAME for messages, falling
// back to the uid when nothing knows it. A member that has no live instance
// still has a registry row, so the name is almost always available — and a bare
// ksuid names nothing the reader has ever been shown.
//
// Exported because internal/mcp needs the same resolution when knomit_repos
// lists a lens member that has no live instance, and a second copy of five
// lines in another package would be free to drift from this one.
//
// LOCKING: takes m.mu (via Repos). NEVER call it from inside a ForEach callback
// or any code path already holding m.mu — sync.RWMutex is not reentrant, and a
// pending writer between the two acquisitions deadlocks the Manager. Same rule
// as validateLensLocked above.
func (m *Manager) RepoLabel(uid string) string {
	reg := m.Repos()
	if reg == nil || uid == "" {
		return uid
	}
	if rec, ok, err := reg.Get(uid); err == nil && ok && rec.Name != "" {
		return rec.Name
	}
	return uid
}
