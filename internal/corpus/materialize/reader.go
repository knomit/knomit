package materialize

import (
	"context"
	"errors"
	"fmt"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// ErrNotReady: a store's reference index cannot be planned on yet (its
// ref_edges backfill has not run AND its repo id is unresolved,
// index.ErrRefEdgesNotReady). Nothing was planned or written; the edge
// answers it as retry-able (503 with Retry-After), never as a failure.
var ErrNotReady = errors.New("the reference index is not ready yet; retry shortly")

// mount is one repo a walk reads: an acquired store at a branch, and the
// commit its LIVE versions are read as of: the walk's anchor for the root's
// repo, the frozen tip for a repo entered cross-KB.
type mount struct {
	ri     *repos.RepoInstance
	id12   string
	branch string
	anchor string
	svc    *store.Service
	// ready: RefEdgesReadiness was checked on this mount.
	ready bool
}

// reader holds the mounts one call touches, acquired on first use and
// released by close. A repo is resolved by its ROOT-COMMIT id: the binding's
// own mount of it first (at the branch the binding pins), else a repo
// registered on the server (repos.Manager.ByRootID, at its read branch) that
// the caller may read (mayRead). Reaching a repo outside the binding is
// default-deny (89b26b07): a repo refused here reads exactly like one that
// does not exist.
type reader struct {
	b        *repos.Binding
	mgr      *repos.Manager
	mayRead  func(repo string) bool
	mounts   map[string]*mount // by id12; a nil value records "not available"
	releases []func()
	// tips are the frozen tips of the repos a cross-KB ref may enter (a
	// FactSet's frozen row); nil resolves a foreign repo's tip on first use
	// (one single-fact walk freezes it for itself).
	tips map[string]frozenTip
	st   *Stats
}

func newReader(b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, st *Stats) *reader {
	if st == nil {
		st = &Stats{}
	}
	return &reader{b: b, mgr: mgr, mayRead: mayRead, mounts: map[string]*mount{}, st: st}
}

func (r *reader) close() {
	for _, rel := range r.releases {
		rel()
	}
	r.releases = nil
}

// target routes a root-commit id to a repo and the branch to read it at.
func (r *reader) target(id12 string) (*repos.RepoInstance, string, bool) {
	if rt, ok := r.b.ByID(id12); ok {
		return rt.RI, rt.Branch, true
	}
	if r.mgr == nil || r.mayRead == nil {
		return nil, "", false
	}
	ri, ok := r.mgr.ByRootID(id12)
	// Not readable by this caller answers exactly like not registered.
	if !ok || !r.mayRead(ri.Name()) {
		return nil, "", false
	}
	return ri, ri.ReadBranch(), true
}

// open acquires the store of ri at branch (once per call).
func (r *reader) open(ri *repos.RepoInstance, branch string) (*mount, error) {
	id12 := fact.ID12(ri.ID())
	if m, ok := r.mounts[id12]; ok && m != nil {
		return m, nil
	}
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, err
	}
	r.releases = append(r.releases, release)
	m := &mount{ri: ri, id12: id12, branch: branch, svc: svc}
	r.mounts[id12] = m
	return m, nil
}

// foreign returns the mount of a repo entered through a cross-KB ref, read at
// its frozen tip, or nil when the repo is not available to this caller (not
// registered, refused by reach, its store not openable, or, in a FactSet, a
// repo whose tip was not frozen at creation).
func (r *reader) foreign(ctx context.Context, id12 string) (*mount, error) {
	if m, ok := r.mounts[id12]; ok {
		return m, nil
	}
	ri, branch, ok := r.target(id12)
	if !ok {
		r.mounts[id12] = nil
		return nil, nil
	}
	var tip frozenTip
	if r.tips != nil {
		t, frozen := r.tips[id12]
		if !frozen {
			r.mounts[id12] = nil
			return nil, nil
		}
		tip = t
		branch = t.Branch
	}
	m, err := r.open(ri, branch)
	if err != nil {
		// Unavailable, not a failure of the walk: the store is closing,
		// populating or gone. The ref reads as not available.
		r.mounts[id12] = nil
		return nil, nil
	}
	if m.anchor == "" {
		if r.tips != nil {
			m.anchor = tip.Commit
		} else {
			wm, err := m.svc.HistoryQuery().Watermark(ctx, m.branch)
			if err != nil {
				return nil, fmt.Errorf("resolve the indexed tip of %s in repo %s: %w", m.branch, id12, err)
			}
			r.st.IndexQueries++
			m.anchor = wm
		}
	}
	return m, nil
}

// checkReady is RefEdgesReadiness on m, once per call: ErrNotReady while the
// store's reference index cannot be planned on.
func (r *reader) checkReady(ctx context.Context, m *mount) error {
	if m.ready {
		return nil
	}
	if err := m.svc.GraphStore().RefEdgesReadiness(ctx); err != nil {
		if errors.Is(err, index.ErrRefEdgesNotReady) {
			return fmt.Errorf("%w (repo %s)", ErrNotReady, m.id12)
		}
		return err
	}
	m.ready = true
	return nil
}

// mountOf is the open mount of the repo with id12, the binding's or a
// reachable registered one at its frozen tip.
func (r *reader) mountOf(ctx context.Context, id12 string) (*mount, error) {
	if m, ok := r.mounts[id12]; ok && m != nil {
		return m, nil
	}
	if id := fact.ID12(r.b.Write().ID()); id == id12 {
		return r.open(r.b.Write(), r.b.WriteMountBranch())
	}
	if rt, ok := r.b.ByID(id12); ok && r.tips == nil {
		return r.open(rt.RI, rt.Branch)
	}
	m, err := r.foreign(ctx, id12)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("repo %s is not available", id12)
	}
	return m, nil
}

// bodies reads the body of each (repo, path, blob) by blob hash, one batched
// object read per repo (index.FactQuery.BlobsByHash): no git object decoded.
func (r *reader) bodies(ctx context.Context, keys []bodyKey) (map[bodyKey]string, error) {
	byRepo := map[string][]bodyKey{}
	var order []string
	for _, k := range keys {
		if k.blob == "" {
			continue
		}
		if _, ok := byRepo[k.repo]; !ok {
			order = append(order, k.repo)
		}
		byRepo[k.repo] = append(byRepo[k.repo], k)
	}
	out := make(map[bodyKey]string, len(keys))
	for _, repo := range order {
		ks := byRepo[repo]
		m, err := r.mountOf(ctx, repo)
		if err != nil {
			return nil, err
		}
		hashes := make([]string, len(ks))
		for i, k := range ks {
			hashes[i] = k.blob
		}
		data, err := m.svc.FactQuery().BlobsByHash(ctx, hashes)
		if err != nil {
			return nil, err
		}
		r.st.IndexQueries++
		r.st.ObjectReads += len(hashes)
		for _, k := range ks {
			raw, ok := data[k.blob]
			if !ok {
				return nil, fmt.Errorf("blob %s of %s is not in repo %s", k.blob, k.rel, repo)
			}
			f, err := fact.ParseFact(k.rel, string(raw))
			if err != nil {
				return nil, fmt.Errorf("parse %s@%s: %w", k.rel, k.blob, err)
			}
			out[k] = f.Body
		}
	}
	return out, nil
}

// bodyKey names the body of one version.
type bodyKey struct{ repo, rel, blob string }

// ReadBodies fills each node's Body from its frozen (repo, blob): how a later
// page of a single-fact materialization gets its bodies. The order and every
// version were frozen by the first call, so a commit landing in between
// changes nothing. A repo no longer reachable through b, or through mgr for a
// caller mayRead admits, is an error: the frozen view cannot be served.
func ReadBodies(ctx context.Context, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, nodes []Node) error {
	r := newReader(b, mgr, mayRead, nil)
	defer r.close()
	keys := make([]bodyKey, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if _, _, ok := r.target(n.Repo); !ok {
			return fmt.Errorf("repo %s is no longer available", n.Repo)
		}
		if n.Blob == "" {
			return fmt.Errorf("%s carries no version", n.Path)
		}
		keys = append(keys, bodyKey{n.Repo, relOf(n.Path), n.Blob})
	}
	got, err := r.bodies(ctx, keys)
	if err != nil {
		return err
	}
	for i := range nodes {
		nodes[i].Body = got[keys[i]]
	}
	return nil
}
