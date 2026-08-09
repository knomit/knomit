// Binding is the runtime object every MCP handler receives instead of a bare
// *RepoInstance (lenses RFC §5.1). It carries the single write target and the
// federated read set (each mount at its pinned branch). Phase 2 keeps reads
// single-target (handlers use Write() only); federation consumes Reads() in
// Phase 3.
package repos

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	name    string
	reads   []ReadTarget
}

// Write returns the single write repo.
func (b *Binding) Write() *RepoInstance { return b.write }

// WriteOK reports whether write tools may operate on this binding. Writes
// always target the write repo's own agent branch (RFC decision 19); a
// lens-of-one bound to a non-writable branch is a read-only view.
func (b *Binding) WriteOK() bool { return b.writeOK }

// WriteMountBranch returns the branch the write repo is READ at in this
// binding (its read mount's pinned branch). Writes never target it — writes
// always go to the write repo's agent branch (RFC decision 19) — but the
// read-only-view error names it, cursor sessions pin it, and the write
// repo's read fan-out searches it.
func (b *Binding) WriteMountBranch() string {
	for _, rt := range b.reads {
		if rt.RI == b.write {
			return rt.Branch
		}
	}
	return b.write.AgentBranch()
}

// Name returns the lens name, or the repo name for a lens-of-one.
func (b *Binding) Name() string { return b.name }

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
// branch: write = repo, reads = [repo@branch], writable iff branch is the
// repo's own agent branch.
func NewBindingOfRepo(ri *RepoInstance, branch string) *Binding {
	if branch == "" {
		branch = ri.AgentBranch()
	}
	return &Binding{
		write:   ri,
		writeOK: ri.WritableBranch(branch),
		name:    ri.Name(),
		reads:   []ReadTarget{{RI: ri, Branch: branch}},
	}
}

// NewBindingForTest builds a binding directly from a write repo and explicit
// read mounts, bypassing lens resolution. Intended for federation/handler
// tests in sibling packages that need a multi-mount binding without standing
// up a full Manager — mirrors NewTestInstanceWithDeps. Production code must
// use NewBindingOfRepo or NewBindingOfLens.
func NewBindingForTest(write *RepoInstance, reads ...ReadTarget) *Binding {
	return &Binding{
		write:   write,
		writeOK: true,
		name:    write.Name(),
		reads:   reads,
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
		return nil, fmt.Errorf("lens %q references unavailable repo %q", l.Name, m.repoLabel(l.WriteUID))
	}
	reads := make([]ReadTarget, 0, len(l.Reads))
	for _, lr := range l.Reads {
		ri := m.GetByUID(lr.RepoUID)
		if ri == nil {
			return nil, fmt.Errorf("lens %q references unavailable repo %q", l.Name, m.repoLabel(lr.RepoUID))
		}
		branch := lr.Branch
		if branch == "" {
			branch = ri.AgentBranch()
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
	return &Binding{
		write:   write,
		writeOK: true, // lens writes always target the write repo's agent branch
		name:    l.Name,
		reads:   reads,
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
// RepoFromContext.
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

// repoLabel resolves a registry uid to the repo NAME for an error message,
// falling back to the uid when nothing knows it. A member that has no live
// instance still has a registry row, so the name is almost always available —
// and a bare ksuid names nothing the reader has ever been shown.
func (m *Manager) repoLabel(uid string) string {
	reg := m.Repos()
	if reg == nil || uid == "" {
		return uid
	}
	if rec, ok, err := reg.Get(uid); err == nil && ok && rec.Name != "" {
		return rec.Name
	}
	return uid
}
