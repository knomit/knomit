package materialize

import (
	"context"
	"errors"
	"fmt"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/model"
)

// mount is one repo the walk reads: an acquired store at a branch, plus the
// commit its LATEST versions are read as of (the walk's anchor for the
// root's repo; the branch tip, resolved once, for a repo entered cross-KB).
type mount struct {
	ri     *repos.RepoInstance
	id12   string
	branch string
	anchor string
	svc    *store.Service
}

// reader holds the mounts one materialization touches, acquired on first use
// and released by close. A repo is resolved by its ROOT-COMMIT id: the
// binding's own mount of it first (at the branch the binding pins), else a
// repo registered on the server (repos.Manager.ByRootID, at its read branch)
// that the caller may read (mayRead).
type reader struct {
	b        *repos.Binding
	mgr      *repos.Manager
	mayRead  func(repo string) bool
	mounts   map[string]*mount // by id12; a nil value records "not available"
	releases []func()
	edges    map[edgeKey]map[string]string
	metas    map[edgeKey]metaEntry
	dates    map[edgeKey]string
	// lives caches LiveRevision per (repo, path, anchor): within a call a
	// path's version live at a fixed anchor never changes.
	lives map[edgeKey]string
	// tips are the frozen tips of repos entered cross-KB, by id12 (a factset
	// seeds them so every unit reads one repo at the tip its first read
	// froze); a repo not in it is resolved once, now.
	tips  map[string]string
	stats WalkStats
}

// WalkStats counts how a reference walk read each version. Index is the fast
// path: the version's row in the index (its blob's facts row: title, type,
// kind, confidence, refs), with no blob read. Parsed is the fallback that
// reads and parses the blob (a version the index never saw, a legacy
// mixed-case path, or one that is read before its commit). Bodies counts the
// bodies read only to decide different_version, which compares bodies, never
// commits; they are compared and dropped.
type WalkStats struct {
	Index  int `json:"index"`
	Parsed int `json:"parsed"`
	Bodies int `json:"bodies"`
}

type edgeKey struct{ repo, rel, commit string }

func newReader(b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool) *reader {
	return &reader{b: b, mgr: mgr, mayRead: mayRead, mounts: map[string]*mount{}, edges: map[edgeKey]map[string]string{},
		metas: map[edgeKey]metaEntry{}, dates: map[edgeKey]string{}, lives: map[edgeKey]string{}}
}

// liveRevision is HistoryQuery.LiveRevision of rel at the mount's anchor,
// cached for the call.
func (r *reader) liveRevision(ctx context.Context, m *mount, rel string) (string, error) {
	k := edgeKey{m.id12, rel, m.anchor}
	if c, ok := r.lives[k]; ok {
		return c, nil
	}
	c, err := m.svc.HistoryQuery().LiveRevision(ctx, m.branch, rel, m.anchor)
	if err != nil {
		return "", err
	}
	r.lives[k] = c
	return c, nil
}

func (r *reader) close() {
	for _, rel := range r.releases {
		rel()
	}
	r.releases = nil
}

// readerCacheMax bounds each version cache of a reader (edges, metadata,
// dates). A version's edges, index row and date never change (a version is
// immutable), so the caches only save repeated reads of facts several units
// share; past the bound a cache is dropped and refilled, so a factset build's
// memory stays bounded whatever the corpus.
const readerCacheMax = 20_000

// forget drops the version caches once one passes readerCacheMax. A factset
// calls it between units: the mounts stay acquired, and facts shared by
// several units are read once while the caches stay small.
func (r *reader) forget() {
	if len(r.edges) > readerCacheMax {
		r.edges = map[edgeKey]map[string]string{}
	}
	if len(r.metas) > readerCacheMax {
		r.metas = map[edgeKey]metaEntry{}
	}
	if len(r.dates) > readerCacheMax {
		r.dates = map[edgeKey]string{}
	}
	if len(r.lives) > readerCacheMax {
		r.lives = map[edgeKey]string{}
	}
}

// metaEntry is one cached index row (hit false: the index cannot answer).
type metaEntry struct {
	meta index.VersionMeta
	hit  bool
}

// versionMeta is VersionMetaAt, cached per (repo, path, commit).
func (r *reader) versionMeta(ctx context.Context, m *mount, rel, commit string) (index.VersionMeta, bool, error) {
	k := edgeKey{m.id12, rel, commit}
	if e, ok := r.metas[k]; ok {
		return e.meta, e.hit, nil
	}
	meta, hit, err := m.svc.FactQuery().VersionMetaAt(ctx, rel, commit)
	if err != nil {
		return index.VersionMeta{}, false, err
	}
	r.metas[k] = metaEntry{meta, hit}
	return meta, hit, nil
}

// prefetchMetas fills the version cache for every pair not cached yet, in
// one batch (index.VersionMetasAt): a pair the batch answers is a hit with
// its row, any other is VersionMetaAt's miss, so versionMeta then answers
// exactly as it would have read by read.
func (r *reader) prefetchMetas(ctx context.Context, m *mount, pairs []index.PathAtCommit) error {
	var need []index.PathAtCommit
	for _, p := range pairs {
		if _, ok := r.metas[edgeKey{m.id12, p.Path, p.Commit}]; !ok {
			need = append(need, p)
		}
	}
	if len(need) == 0 {
		return nil
	}
	got, err := m.svc.FactQuery().VersionMetasAt(ctx, need)
	if err != nil {
		return err
	}
	for _, p := range need {
		meta, hit := got[p]
		r.metas[edgeKey{m.id12, p.Path, p.Commit}] = metaEntry{meta, hit}
	}
	return nil
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
	// Not readable by this caller answers exactly like not registered: the
	// two must stay indistinguishable, or a caller could probe which repos
	// exist.
	if !ok || !r.mayRead(ri.Name()) {
		return nil, "", false
	}
	return ri, ri.ReadBranch(), true
}

// open acquires the store of the repo with id12 at branch. anchor "" leaves
// the mount's anchor to be set by the caller.
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

// foreign returns the mount for a repo entered through a cross-KB ref, read
// at its branch tip (resolved once and frozen for the whole walk), or nil
// when no registered repo has the id or its store cannot be opened.
func (r *reader) foreign(ctx context.Context, id12 string) (*mount, error) {
	if m, ok := r.mounts[id12]; ok {
		return m, nil
	}
	ri, branch, ok := r.target(id12)
	if !ok {
		r.mounts[id12] = nil
		return nil, nil
	}
	m, err := r.open(ri, branch)
	if err != nil {
		// Unavailable, not a failure of the walk: the store is closing,
		// populating or gone. The ref is reported as not available.
		r.mounts[id12] = nil
		return nil, nil
	}
	if m.anchor == "" && r.tips[id12] != "" {
		// A factset froze this repo's tip when an earlier unit first read it.
		m.anchor = r.tips[id12]
	}
	if m.anchor == "" {
		tip, err := m.svc.HeadCommit(ctx, m.branch)
		if err != nil {
			return nil, fmt.Errorf("resolve the tip of %s in repo %s: %w", m.branch, id12, err)
		}
		m.anchor = tip
	}
	return m, nil
}

// readAt reads the METADATA of path at commit: title, type, kind, confidence
// and refs, with Body left empty (the walk never holds a body). A version
// that is not in commit's tree falls back to the latest version before it,
// as knomit_explain does for a target retracted after it was cited: the
// cited content is still what the referrer reasoned over. used is the commit
// the content came from; blob is its blob hash, "" when only the fallback
// could read it.
func (r *reader) readAt(ctx context.Context, m *mount, rel, commit string) (f fact.Fact, used, blob string, ok bool, err error) {
	meta, hit, err := r.versionMeta(ctx, m, rel, commit)
	if err != nil {
		return fact.Fact{}, "", "", false, err
	}
	if hit {
		r.stats.Index++
		return metaFact(rel, meta), commit, meta.Blob, true, nil
	}
	r.stats.Parsed++
	f, used, ok, err = r.parseAt(ctx, m, rel, commit)
	f.Body = ""
	return f, used, "", ok, err
}

// metaFact is an index row as a body-less fact.
func metaFact(rel string, meta index.VersionMeta) fact.Fact {
	f := fact.NewFact(rel)
	f.Title, f.Kind, f.Type = meta.Title, fact.Kind(meta.Kind), fact.Type(meta.Type)
	f.Confidence, f.Refs = meta.Confidence, meta.Refs
	return f
}

// parseAt reads and parses the blob of path at commit, with readAt's
// before-the-commit fallback; the body is included.
func (r *reader) parseAt(ctx context.Context, m *mount, rel, commit string) (f fact.Fact, used string, ok bool, err error) {
	res, err := m.svc.Facts().ReadFact(ctx, m.branch, rel, &facts.ReadFactOpts{AtCommit: commit})
	used = commit
	if errors.Is(err, model.ErrPathNotFound) {
		res, err = m.svc.Facts().ReadFact(ctx, m.branch, rel, &facts.ReadFactOpts{BeforeCommit: commit})
		used = res.FromCommit
	}
	if errors.Is(err, model.ErrPathNotFound) {
		return fact.Fact{}, "", false, nil
	}
	if err != nil {
		return fact.Fact{}, "", false, fmt.Errorf("read %s at %s: %w", rel, commit, err)
	}
	f, err = fact.ParseFact(rel, res.Content)
	if err != nil {
		return fact.Fact{}, "", false, fmt.Errorf("parse %s at %s: %w", rel, used, err)
	}
	return f, used, true, nil
}

// bodiesDiffer reports whether two versions of one fact differ in BODY (the
// "changed since cited" rule: never commit inequality alone). The same
// commit or the same blob is the same body without a read; otherwise both
// bodies are read, compared and dropped.
func (r *reader) bodiesDiffer(ctx context.Context, m *mount, rel, commitA, blobA, commitB, blobB string) (bool, error) {
	if commitA == commitB || (blobA != "" && blobA == blobB) {
		return false, nil
	}
	r.stats.Bodies += 2
	a, _, okA, err := r.parseAt(ctx, m, rel, commitA)
	if err != nil {
		return false, err
	}
	b, _, okB, err := r.parseAt(ctx, m, rel, commitB)
	if err != nil {
		return false, err
	}
	return okA && okB && a.Body != b.Body, nil
}

// latest reads the METADATA of path's version live at the mount's anchor. ok
// is false when the path is not in the anchor's tree: retracted, deleted or
// never written by then. commit is the change that introduced that version
// (first-parent resolution, never wall-clock), not the anchor itself.
func (r *reader) latest(ctx context.Context, m *mount, rel string) (f fact.Fact, commit, blob string, ok bool, err error) {
	meta, hit, err := r.versionMeta(ctx, m, rel, m.anchor)
	if err != nil {
		return fact.Fact{}, "", "", false, err
	}
	if hit {
		r.stats.Index++
		f, blob = metaFact(rel, meta), meta.Blob
	} else {
		r.stats.Parsed++
		res, err := m.svc.Facts().ReadFact(ctx, m.branch, rel, &facts.ReadFactOpts{AtCommit: m.anchor})
		if errors.Is(err, model.ErrPathNotFound) {
			return fact.Fact{}, "", "", false, nil
		}
		if err != nil {
			return fact.Fact{}, "", "", false, fmt.Errorf("read %s at %s: %w", rel, m.anchor, err)
		}
		if f, err = fact.ParseFact(rel, res.Content); err != nil {
			return fact.Fact{}, "", "", false, fmt.Errorf("parse %s at %s: %w", rel, m.anchor, err)
		}
		f.Body = ""
	}
	commit, err = r.liveRevision(ctx, m, rel)
	if err != nil {
		return fact.Fact{}, "", "", false, fmt.Errorf("resolve the version of %s at %s: %w", rel, m.anchor, err)
	}
	if commit == "" {
		// Present in the tree but below the indexed history (a history
		// boundary): the anchor is the only commit known to hold it.
		commit = m.anchor
	}
	return f, commit, blob, true, nil
}

// pins returns the versions (path, commit) cites: the target commit of each
// DERIVED_FROM edge out of that version, by repo-relative target path.
func (r *reader) pins(ctx context.Context, m *mount, rel, commit string) (map[string]string, error) {
	k := edgeKey{m.id12, rel, commit}
	if p, ok := r.edges[k]; ok {
		return p, nil
	}
	out, err := m.svc.GraphStore().OutgoingAtCommit(ctx, m.branch, rel, commit)
	if err != nil {
		return nil, fmt.Errorf("outgoing refs of %s at %s: %w", rel, commit, err)
	}
	p := make(map[string]string, len(out))
	for _, e := range out {
		if _, dup := p[e.Path]; !dup {
			p[e.Path] = e.Commit
		}
	}
	r.edges[k] = p
	return p, nil
}

// date is the yyyy-mm-dd commit date of the version of path at commit, or ""
// when the index has no row for it.
func (r *reader) date(ctx context.Context, m *mount, rel, commit string) string {
	k := edgeKey{m.id12, rel, commit}
	if d, ok := r.dates[k]; ok {
		return d
	}
	d := ""
	revs, err := m.svc.HistoryQuery().RevisionsBefore(ctx, m.branch, rel, commit, 1)
	if err != nil {
		return "" // not cached: a failed lookup is retried
	}
	if len(revs) > 0 && revs[0].CommittedAt != 0 {
		d = time.Unix(revs[0].CommittedAt, 0).UTC().Format("2006-01-02")
	}
	r.dates[k] = d
	return d
}

// ReadBodies fills each node's Body by reading it at its stored (repo,
// commit). It is how a later page of a paged materialization gets its
// bodies: the order and every version were frozen by the first call, so a
// commit landing in between changes nothing. A repo that is no longer
// reachable through b, or through mgr for a caller mayRead admits, is an
// error: the frozen view cannot be served.
func ReadBodies(ctx context.Context, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, nodes []Node) error {
	r := newReader(b, mgr, mayRead)
	defer r.close()
	return r.readBodies(ctx, nodes)
}

// readBodies is ReadBodies on an open reader.
func (r *reader) readBodies(ctx context.Context, nodes []Node) error {
	for i := range nodes {
		n := &nodes[i]
		ri, branch, ok := r.target(n.Repo)
		if !ok {
			return fmt.Errorf("repo %s is no longer available", n.Repo)
		}
		m, err := r.open(ri, branch)
		if err != nil {
			return fmt.Errorf("repo %s: %w", n.Repo, err)
		}
		_, rel, _, err := fact.ParseKBPath(n.Path)
		if err != nil {
			return err
		}
		f, _, ok, err := r.parseAt(ctx, m, rel, n.Commit)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s is not readable at %s", n.Path, n.Commit)
		}
		n.Body = f.Body
	}
	return nil
}
