package store

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	storegit "knomit/internal/store/git"
)

// Per-path change history, precomputed (tables: migration 000032).
//
// A path's history is the set of ENTRIES — commits that introduced a blob for
// the path that none of their parents had — linked to the entries they were
// edited from. See kb/decisions/mcp/explain/history-enumeration.
//
// DERIVATION DEPENDS ON GIT ALONE. A commit's rows (path_changes, links, and
// its commit_fp depth and jump pointers) are computed from its git objects —
// its tree diff against its first parent, its and its parents' blobs — and the
// rows of its ancestors, never from branch_commits or commit_log. Deriving a
// commit first derives its underived ancestors, walking git parents first
// (deriveClosure). Rows are IMMUTABLE per hash: nothing deletes them on a
// rewind or a rebuild. A pathChangesVersion bump re-derives into shadow tables
// and flips; a DEGRADED row (derived around a missing object) is re-derived
// by :rebuild.
//
// WHEN: a commit's rows are derived before the ref that makes it visible
// advances (deriveBeforeAdvance), and the transaction that records it in
// branch_commits refuses it otherwise (CommitLogApply's hook). An indexed
// commit is never visible without its rows; knomit_explain never derives.
//
// LOCKING: every git object read (and diff) is PREPARED outside any
// transaction; inside, apply runs SQL only — enforced: a git read while
// applying is an error (errReadInApply).
//
// MISSING OBJECTS (user decision) never fail a derivation, so they never fail
// a write:
//   - a missing parent COMMIT is a history boundary (like a shallow clone's):
//     the child is `added`, with no link below;
//   - a missing BLOB, or a missing tree on the path, records the revision and
//     its links but marks it content_unavailable, with no diff.
//
// Both mark the commit degraded (commit_fp.degraded) so :rebuild re-derives it
// once the object is back. Any OTHER read error fails the derivation, nothing
// is recorded, and a later attempt retries — before the ref moves.

// pathChangesVersion is the version of the derivation below. A stored value
// that differs (or none) makes the next populate re-derive into shadow
// tables and flip them in.
const pathChangesVersion = "1"

const pathChangesVersionKey = "path_changes_version"
const pathChangesShadowKey = "path_changes_shadow_version"

// pathChangeBatch is how many commits one short transaction derives: one
// commit per transaction would cost one WAL commit each; more per batch holds
// the write lock longer for concurrent writers (a 1000-commit batch held it
// ~1 s).
const pathChangeBatch = 250

// Degradation marks on commit_fp.degraded.
const (
	degradedNone       = 0
	degradedContent    = 1 // a revision of this commit is content_unavailable
	degradedStructural = 2 // a parent commit was missing, here or below: depth and links are cut
)

// ErrHistoryChanged is returned for a PathHistory continuation whose anchor
// has left the branch, or whose position names a change no longer on the
// branch: the history it was paging is gone, and the caller must restart.
var ErrHistoryChanged = errors.New("path history changed since this position was issued")

// errParentUnderived is returned by apply for a commit whose parent (present
// in the object store) is not derived. deriveClosure orders parents first, so
// it signals a bug, never a state to record around.
var errParentUnderived = errors.New("parent commit is not derived yet")

// errNotDerived is returned by the recording hook for a commit whose rows are
// not derived: recording it would make it visible without its history.
var errNotDerived = errors.New("commit is not derived")

// errReadInApply guards apply's contract: SQL only, never a git read (a
// write transaction waiting for a pool connection starves other writers).
var errReadInApply = errors.New("git object read inside apply")

// deriveObjectHook, when set (tests only), runs before every git object read
// of the derivation; a non-nil error is returned as that read's error.
var deriveObjectHook func(kind string, h plumbing.Hash) error

// pcTables names one set of derived tables: the served set, or the shadow set
// a version bump derives into before flipping.
type pcTables struct{ changes, links, fp string }

var (
	activeTables = pcTables{"path_changes", "path_change_links", "commit_fp"}
	shadowTables = pcTables{"path_changes_next", "path_change_links_next", "commit_fp_next"}
)

// createTablesSQL is the DDL of a table set (migration 000032 creates the
// active one with the same columns). indexSuffix keeps index names unique
// across flips.
func createTablesSQL(t pcTables, indexSuffix string) []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + t.changes + ` (
		    path        TEXT    NOT NULL,
		    commit_hash TEXT    NOT NULL,
		    entry       INTEGER NOT NULL,
		    resolves_to TEXT    NOT NULL,
		    blob        TEXT    NOT NULL,
		    action      TEXT    NOT NULL DEFAULT '',
		    author_at   INTEGER NOT NULL DEFAULT 0,
		    order_at    INTEGER NOT NULL DEFAULT 0,
		    gen         INTEGER NOT NULL DEFAULT 0,
		    message     TEXT    NOT NULL DEFAULT '',
		    diff        TEXT    NOT NULL DEFAULT '',
		    fp_depth    INTEGER NOT NULL DEFAULT 0,
		    content_unavailable INTEGER NOT NULL DEFAULT 0,
		    PRIMARY KEY (path, commit_hash))`,
		`CREATE INDEX IF NOT EXISTS ` + t.changes + `_depth_` + indexSuffix + ` ON ` + t.changes + ` (path, fp_depth)`,
		`CREATE TABLE IF NOT EXISTS ` + t.links + ` (
		    path         TEXT    NOT NULL,
		    commit_hash  TEXT    NOT NULL,
		    parent_order INTEGER NOT NULL,
		    from_commit  TEXT    NOT NULL,
		    from_blob    TEXT    NOT NULL,
		    PRIMARY KEY (path, commit_hash, parent_order))`,
		`CREATE TABLE IF NOT EXISTS ` + t.fp + ` (
		    commit_hash TEXT    PRIMARY KEY,
		    depth       INTEGER NOT NULL,
		    ups         BLOB    NOT NULL,
		    degraded    INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS ` + t.fp + `_degraded_` + indexSuffix + ` ON ` + t.fp + ` (degraded) WHERE degraded > 0`,
	}
}

func metaGet(ctx context.Context, q storegit.CtxExecer, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// ensureAllDerived brings the served tables to the current derivation
// version for every commit reachable from any branch tip. With the version
// current it is one lookup. Otherwise — an upgraded database, or a bump — it
// derives every tip's history into the SHADOW tables (resumable: an
// interrupted pass continues where it stopped), then flips them in with the
// version in one short transaction. Until the flip the old tables keep being
// served; a failed pass leaves them untouched. A branch whose ref cannot be
// resolved (other than "no such ref") blocks the flip and is logged.
func (rh *repoHandler) ensureAllDerived(ctx context.Context) error {
	if rh.repo == nil {
		return nil
	}
	q := conn(ctx, rh.db)
	v, err := metaGet(ctx, q, pathChangesVersionKey)
	if err != nil {
		return fmt.Errorf("path changes: version: %w", err)
	}
	if v == pathChangesVersion {
		return nil
	}
	start := time.Now()
	sv, err := metaGet(ctx, q, pathChangesShadowKey)
	if err != nil {
		return fmt.Errorf("path changes: shadow version: %w", err)
	}
	if sv != pathChangesVersion {
		for _, stmt := range []string{`DROP TABLE IF EXISTS ` + shadowTables.changes, `DROP TABLE IF EXISTS ` + shadowTables.links, `DROP TABLE IF EXISTS ` + shadowTables.fp} {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("path changes: shadow reset: %w", err)
			}
		}
	}
	for _, stmt := range createTablesSQL(shadowTables, "v"+pathChangesVersion) {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("path changes: shadow tables: %w", err)
		}
	}
	if _, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, pathChangesShadowKey, pathChangesVersion); err != nil {
		return fmt.Errorf("path changes: shadow version: %w", err)
	}

	tips, ok, err := rh.branchTips(ctx)
	if err != nil {
		return err
	}
	n, err := rh.deriveClosure(ctx, newDeriver(rh, shadowTables), tips)
	if err != nil {
		return err
	}
	if !ok {
		log.Warn().Msg("path changes: a branch ref could not be resolved; the new derivation is NOT flipped in (old history stays served)")
		return nil
	}

	// Flip: one short transaction.
	ctx, tx, own, err := beginTxIfNeeded(ctx, rh.db)
	if err != nil {
		return fmt.Errorf("path changes: flip: %w", err)
	}
	if own {
		defer tx.Rollback() //nolint:errcheck
	}
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS ` + activeTables.changes,
		`DROP TABLE IF EXISTS ` + activeTables.links,
		`DROP TABLE IF EXISTS ` + activeTables.fp,
		`ALTER TABLE ` + shadowTables.changes + ` RENAME TO ` + activeTables.changes,
		`ALTER TABLE ` + shadowTables.links + ` RENAME TO ` + activeTables.links,
		`ALTER TABLE ` + shadowTables.fp + ` RENAME TO ` + activeTables.fp,
		`DELETE FROM meta WHERE key = '` + pathChangesShadowKey + `'`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("path changes: flip: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, pathChangesVersionKey, pathChangesVersion); err != nil {
		return fmt.Errorf("path changes: flip: %w", err)
	}
	if own {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("path changes: flip: %w", err)
		}
	}
	log.Info().Int("commits", n).Dur("elapsed", time.Since(start)).Msg("path changes: derived and flipped in")
	return nil
}

// branchTips resolves every registered branch's tip. ok is false when a ref
// failed to resolve for a reason other than not existing (logged): a branch
// with no ref has no history to derive.
func (rh *repoHandler) branchTips(ctx context.Context) ([]plumbing.Hash, bool, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx, `SELECT name FROM branches`)
	if err != nil {
		return nil, false, fmt.Errorf("path changes: branches: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, false, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	ok := true
	var tips []plumbing.Hash
	for _, n := range names {
		h, err := rh.resolveRef(ctx, n)
		if err != nil {
			if !errors.Is(err, plumbing.ErrReferenceNotFound) {
				log.Warn().Err(err).Str("branch", n).Msg("path changes: branch ref unresolved")
				ok = false
			}
			continue
		}
		tips = append(tips, h)
	}
	return tips, ok, nil
}

// deriveBeforeAdvance derives hash (and any underived ancestors) into the
// served tables — called BEFORE a ref moves to hash, so a derivation failure
// leaves the ref where it was. Missing objects never fail it (they become a
// boundary or content_unavailable); only other read errors do.
func (rh *repoHandler) deriveBeforeAdvance(ctx context.Context, hash plumbing.Hash) error {
	if rh.repo == nil || !rh.gits.CommitLogAvailable() {
		return nil
	}
	_, err := rh.deriveClosure(ctx, newDeriver(rh, activeTables), []plumbing.Hash{hash})
	return err
}

// deriveClosure derives every underived commit reachable from starts through
// git parents (starts included), parents first, in short transactions of
// pathChangeBatch commits, each prepared from git before it opens. It stops at
// derived commits and at commits absent from the object store. A read error
// other than not-found fails it; what earlier batches committed stays.
// Returns how many commits it derived.
func (rh *repoHandler) deriveClosure(ctx context.Context, d *deriver, starts []plumbing.Hash) (int, error) {
	q := conn(ctx, rh.db)
	var pending []*object.Commit
	seen := map[plumbing.Hash]bool{}
	stack := append([]plumbing.Hash(nil), starts...)
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		seen[h] = true
		marked, err := d.isDerived(ctx, q, h.String())
		if err != nil {
			return 0, err
		}
		if marked {
			continue
		}
		c, err := d.commit(h)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue // a boundary: no history below
		}
		if err != nil {
			return 0, err
		}
		pending = append(pending, c)
		stack = append(stack, c.ParentHashes...)
	}
	return len(pending), rh.deriveCommits(ctx, d, parentsFirst(pending), false)
}

// deriveCommits prepares and applies commits (parents first) in batched
// transactions. With replace, existing rows of each commit are replaced (the
// re-derivation of degraded commits); otherwise a derived commit is left as is.
func (rh *repoHandler) deriveCommits(ctx context.Context, d *deriver, order []*object.Commit, replace bool) error {
	for i := 0; i < len(order); i += pathChangeBatch {
		chunk := order[i:min(i+pathChangeBatch, len(order))]
		prepared := make([]*preparedCommit, len(chunk))
		for j, c := range chunk {
			var err error
			if prepared[j], err = d.prepare(ctx, c); err != nil {
				return err
			}
		}
		bctx, tx, own, err := beginTxIfNeeded(ctx, rh.db)
		if err != nil {
			return fmt.Errorf("path changes: begin: %w", err)
		}
		for _, p := range prepared {
			if replace {
				err = d.clear(bctx, tx, p.c.Hash.String())
			}
			if err == nil {
				err = d.apply(bctx, tx, p)
			}
			if err != nil {
				if own {
					tx.Rollback() //nolint:errcheck
				}
				return err
			}
		}
		if own {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("path changes: commit: %w", err)
			}
		}
	}
	return nil
}

// rederiveDegraded re-derives, in the served tables, every commit derived
// around a missing object — so a boundary or a content_unavailable revision
// is repaired once the object is back (:rebuild). A commit whose object is
// still missing stays degraded.
func (rh *repoHandler) rederiveDegraded(ctx context.Context) (int, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx, `SELECT commit_hash FROM `+activeTables.fp+` WHERE degraded > 0`)
	if err != nil {
		return 0, fmt.Errorf("path changes: degraded: %w", err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return 0, err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(hashes) == 0 {
		return 0, err
	}
	d := newDeriver(rh, activeTables)
	var commits []*object.Commit
	for _, h := range hashes {
		c, err := d.commit(plumbing.NewHash(h))
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		commits = append(commits, c)
	}
	// A re-derived commit's parents may be underived (they were missing
	// before): derive them first, then replace the degraded commits.
	var parents []plumbing.Hash
	for _, c := range commits {
		parents = append(parents, c.ParentHashes...)
	}
	if _, err := rh.deriveClosure(ctx, d, parents); err != nil {
		return 0, err
	}
	return len(commits), rh.deriveCommits(ctx, d, parentsFirst(commits), true)
}

// markdownChanges picks the .md add/modify paths out of a commit's changes,
// exactly as commit_log stores them: one row per (commit, lowercased path),
// the FIRST entry winning — a case-only rename yields a delete and an add of
// the same lowercased path.
func markdownChanges(entries []storegit.CommitLogEntry) []string {
	seen := map[string]bool{}
	var paths []string
	for _, e := range entries {
		if seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		if (e.Action == "added" || e.Action == "modified") && fact.IsMarkdownPath(e.Path) {
			paths = append(paths, e.Path)
		}
	}
	return paths
}

// parentsFirst orders commits so every commit follows those of its parents
// that are in the set (Kahn; ties in input order).
func parentsFirst(commits []*object.Commit) []*object.Commit {
	in := make(map[plumbing.Hash]bool, len(commits))
	for _, c := range commits {
		in[c.Hash] = true
	}
	indeg := map[plumbing.Hash]int{}
	children := map[plumbing.Hash][]*object.Commit{}
	for _, c := range commits {
		for _, p := range c.ParentHashes {
			if in[p] {
				indeg[c.Hash]++
				children[p] = append(children[p], c)
			}
		}
	}
	order := make([]*object.Commit, 0, len(commits))
	for _, c := range commits {
		if indeg[c.Hash] == 0 {
			order = append(order, c)
		}
	}
	for i := 0; i < len(order); i++ {
		for _, ch := range children[order[i].Hash] {
			if indeg[ch.Hash]--; indeg[ch.Hash] == 0 {
				order = append(order, ch)
			}
		}
	}
	return order
}

// deriver derives commits into one table set. It keeps git object caches for
// the life of one populate or pass. prepare reads git; apply writes SQL on
// the transaction it is handed and may not read git.
type deriver struct {
	rh       *repoHandler
	t        pcTables
	applying bool
	trees    map[plumbing.Hash]*object.Tree
	commits  map[plumbing.Hash]*object.Commit
	facts    map[plumbing.Hash]*fact.Fact // parsed blobs; nil when not a fact
}

func newDeriver(rh *repoHandler, t pcTables) *deriver {
	return &deriver{rh: rh, t: t, trees: map[plumbing.Hash]*object.Tree{}, commits: map[plumbing.Hash]*object.Commit{}, facts: map[plumbing.Hash]*fact.Fact{}}
}

func (d *deriver) readGuard(kind string, h plumbing.Hash) error {
	if d.applying {
		return fmt.Errorf("%w: %s %s", errReadInApply, kind, h)
	}
	if deriveObjectHook != nil {
		return deriveObjectHook(kind, h)
	}
	return nil
}

func (d *deriver) isDerived(ctx context.Context, q storegit.CtxExecer, hash string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+d.t.fp+` WHERE commit_hash = ?`, hash).Scan(&n); err != nil {
		return false, fmt.Errorf("path changes: mark: %w", err)
	}
	return n > 0, nil
}

// commit loads a commit; an absent one is an error wrapping
// plumbing.ErrObjectNotFound.
func (d *deriver) commit(h plumbing.Hash) (*object.Commit, error) {
	if d.applying {
		return nil, fmt.Errorf("%w: commit %s", errReadInApply, h)
	}
	if c, ok := d.commits[h]; ok {
		return c, nil
	}
	if err := d.readGuard("commit", h); err != nil {
		return nil, err
	}
	c, err := d.rh.repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("path changes: commit %s: %w", h, err)
	}
	if len(d.commits) > 4096 {
		clear(d.commits)
	}
	d.commits[h] = c
	return c, nil
}

// tree loads a tree; an absent one is an error wrapping
// plumbing.ErrObjectNotFound.
func (d *deriver) tree(h plumbing.Hash) (*object.Tree, error) {
	if d.applying {
		return nil, fmt.Errorf("%w: tree %s", errReadInApply, h)
	}
	if t, ok := d.trees[h]; ok {
		return t, nil
	}
	if err := d.readGuard("tree", h); err != nil {
		return nil, err
	}
	t, err := d.rh.repo.TreeObject(h)
	if err != nil {
		return nil, fmt.Errorf("path changes: tree %s: %w", h, err)
	}
	if len(d.trees) > 16384 {
		clear(d.trees)
	}
	d.trees[h] = t
	return t, nil
}

// parsed returns the fact in a blob, parsed once per deriver: nil when the
// content is not a fact. missing reports a blob absent from the store.
func (d *deriver) parsed(path string, h plumbing.Hash) (f *fact.Fact, missing bool, err error) {
	if d.applying {
		return nil, false, fmt.Errorf("%w: blob %s", errReadInApply, h)
	}
	if f, ok := d.facts[h]; ok {
		return f, false, nil
	}
	if err := d.readGuard("blob", h); err != nil {
		return nil, false, err
	}
	b, err := d.rh.repo.BlobObject(h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("path changes: blob %s: %w", h, err)
	}
	r, err := b.Reader()
	if err != nil {
		return nil, false, fmt.Errorf("path changes: blob %s: %w", h, err)
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return nil, false, fmt.Errorf("path changes: blob %s: %w", h, err)
	}
	var out *fact.Fact
	if pf, err := fact.ParseFact(path, string(raw)); err == nil {
		out = &pf
	}
	if len(d.facts) > 1024 {
		clear(d.facts)
	}
	d.facts[h] = out
	return out, false, nil
}

// blobState is what a tree says about a path.
type blobState int

const (
	blobAbsent  blobState = iota // the path is not in the tree
	blobPresent                  // hash is the path's blob
	blobUnknown                  // a tree on the path is missing from the store
)

// blobAt looks path up in the tree rooted at rootHash, case-insensitively
// (fact paths are lowercase-canonical; the tree may not be). Only a read
// error other than not-found is an error.
func (d *deriver) blobAt(rootHash plumbing.Hash, path string) (plumbing.Hash, blobState, error) {
	root, err := d.tree(rootHash)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return plumbing.ZeroHash, blobUnknown, nil
	}
	if err != nil {
		return plumbing.ZeroHash, blobUnknown, err
	}
	e, err := treeEntryInsensitiveVia(d.tree, root, path)
	switch {
	case err == nil:
		return e.Hash, blobPresent, nil
	case errors.Is(err, ErrPathNotFound):
		return plumbing.ZeroHash, blobAbsent, nil
	case errors.Is(err, plumbing.ErrObjectNotFound):
		return plumbing.ZeroHash, blobUnknown, nil
	default:
		return plumbing.ZeroHash, blobUnknown, err
	}
}

// diffTrees appends the changes from the tree `from` to the tree `to`
// (either zero for an empty tree) under prefix, in the order git's tree diff
// emits them. Subtrees with equal hashes are skipped without being read, so
// the work is proportional to what changed. incomplete reports a tree that is
// missing from the store (its changes cannot be listed).
func (d *deriver) diffTrees(from, to plumbing.Hash, prefix string, out *[]storegit.CommitLogEntry) (incomplete bool, err error) {
	if from == to {
		return false, nil
	}
	load := func(h plumbing.Hash) ([]object.TreeEntry, bool, error) {
		if h.IsZero() {
			return nil, false, nil
		}
		t, err := d.tree(h)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		return t.Entries, false, nil
	}
	fe, fmiss, err := load(from)
	if err != nil {
		return false, err
	}
	te, tmiss, err := load(to)
	if err != nil {
		return false, err
	}
	if fmiss || tmiss {
		return true, nil
	}
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	emit := func(name, action string) {
		*out = append(*out, storegit.CommitLogEntry{Path: strings.ToLower(prefix + name), Action: action})
	}
	i, j := 0, 0
	for i < len(fe) || j < len(te) {
		var f, t *object.TreeEntry
		switch {
		case j >= len(te) || (i < len(fe) && key(fe[i]) < key(te[j])):
			f = &fe[i]
			i++
		case i >= len(fe) || key(te[j]) < key(fe[i]):
			t = &te[j]
			j++
		default:
			f, t = &fe[i], &te[j]
			i++
			j++
		}
		var inc bool
		switch {
		case f != nil && t != nil:
			if f.Hash == t.Hash && f.Mode == t.Mode {
				continue
			}
			if f.Mode == filemode.Dir {
				inc, err = d.diffTrees(f.Hash, t.Hash, prefix+f.Name+"/", out)
			} else if f.Mode != filemode.Submodule {
				emit(t.Name, "modified")
			}
		case f != nil:
			if f.Mode == filemode.Dir {
				inc, err = d.diffTrees(f.Hash, plumbing.ZeroHash, prefix+f.Name+"/", out)
			} else if f.Mode != filemode.Submodule {
				emit(f.Name, "deleted")
			}
		default:
			if t.Mode == filemode.Dir {
				inc, err = d.diffTrees(plumbing.ZeroHash, t.Hash, prefix+t.Name+"/", out)
			} else if t.Mode != filemode.Submodule {
				emit(t.Name, "added")
			}
		}
		if err != nil {
			return false, err
		}
		incomplete = incomplete || inc
	}
	return incomplete, nil
}

// preparedCommit is everything deriving a commit needs from git objects —
// read before any transaction opens. apply turns it into rows with SQL only.
type preparedCommit struct {
	c            *object.Commit
	parentAbsent []bool // parent i is not in the object store (a boundary)
	degraded     int
	paths        []preparedPath
}

type preparedPath struct {
	path        string
	to          plumbing.Hash   // zero when unknown (content_unavailable)
	blobs       []plumbing.Hash // per parent; zero when the parent lacks the path or it is unknown
	parentHas   []bool          // per parent: has the path (a known or an unknown blob)
	sameAs      int             // the first parent whose blob equals to, or -1
	diff        string          // RevisionDiff JSON against the first parent that has the path
	unavailable bool            // content or its diff base is missing from the store
}

// prepare reads from git everything apply needs for c: its .md changes
// against its first parent (the tree diff, always — never commit_log's
// recorded rows, which can drift from git), each changed path's blob at c and
// at every parent, and the diff an entry stores. Missing objects are recorded
// as boundaries or content_unavailable; other read errors fail. commit_log is
// consulted only for a commit whose trees are missing — the one case git
// cannot list the changed paths — and the commit is then degraded.
func (d *deriver) prepare(ctx context.Context, c *object.Commit) (*preparedCommit, error) {
	pc := &preparedCommit{c: c, parentAbsent: make([]bool, len(c.ParentHashes))}
	parentTrees := make([]plumbing.Hash, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		pcm, err := d.commit(p)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			pc.parentAbsent[i] = true
			pc.degraded = degradedStructural
			continue
		}
		if err != nil {
			return nil, err
		}
		parentTrees[i] = pcm.TreeHash
	}
	var entries []storegit.CommitLogEntry
	{
		var from plumbing.Hash
		if len(parentTrees) > 0 {
			from = parentTrees[0]
		}
		incomplete, err := d.diffTrees(from, c.TreeHash, "", &entries)
		if err != nil {
			return nil, err
		}
		if incomplete {
			// A tree needed for the diff is missing: take the changed paths
			// commit_log recorded when git still had it.
			pc.degraded = max(pc.degraded, degradedContent)
			rows, err := conn(ctx, d.rh.db).QueryContext(ctx, `SELECT path, action FROM commit_log WHERE commit_hash = ?`, c.Hash.String())
			if err != nil {
				return nil, fmt.Errorf("path changes: recorded diff: %w", err)
			}
			for rows.Next() {
				var e storegit.CommitLogEntry
				if err := rows.Scan(&e.Path, &e.Action); err != nil {
					rows.Close()
					return nil, err
				}
				entries = append(entries, e)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
	}
	for _, path := range markdownChanges(entries) {
		to, st, err := d.blobAt(c.TreeHash, path)
		if err != nil {
			return nil, err
		}
		if st == blobAbsent {
			continue
		}
		pp := preparedPath{path: path, blobs: make([]plumbing.Hash, len(c.ParentHashes)), parentHas: make([]bool, len(c.ParentHashes)), sameAs: -1}
		if st == blobPresent {
			pp.to = to
		} else {
			pp.unavailable = true
		}
		for i := range c.ParentHashes {
			if pc.parentAbsent[i] {
				continue
			}
			b, pst, err := d.blobAt(parentTrees[i], path)
			if err != nil {
				return nil, err
			}
			switch pst {
			case blobPresent:
				pp.blobs[i], pp.parentHas[i] = b, true
				if !pp.to.IsZero() && b == pp.to && pp.sameAs < 0 {
					// Content carried over from parent i. i == 0 is a
					// case-only rename (commit_log records it; the blob is
					// unchanged): still a row, so rows stay one-to-one with
					// commit_log's add/modify rows.
					pp.sameAs = i
				}
			case blobUnknown:
				pp.parentHas[i] = true
				if i == firstWithPath(pp.parentHas) {
					pp.unavailable = true // the diff base is unknown
				}
			}
		}
		if pp.sameAs < 0 && !pp.unavailable {
			if base := firstWithPath(pp.parentHas); base >= 0 {
				cur, missCur, err := d.parsed(path, pp.to)
				if err != nil {
					return nil, err
				}
				prev, missPrev, err := d.parsed(path, pp.blobs[base])
				if err != nil {
					return nil, err
				}
				if missCur || missPrev {
					pp.unavailable = true
				} else if cur != nil {
					if rd := revisionDelta(prev, *cur); rd != nil {
						js, _ := json.Marshal(rd)
						pp.diff = string(js)
					}
				}
			} else if !pp.to.IsZero() {
				// An addition: nothing to diff, but its content must exist.
				_, miss, err := d.parsed(path, pp.to)
				if err != nil {
					return nil, err
				}
				pp.unavailable = miss
			}
		}
		if pp.unavailable {
			pc.degraded = max(pc.degraded, degradedContent)
		}
		pc.paths = append(pc.paths, pp)
	}
	return pc, nil
}

func firstWithPath(has []bool) int {
	for i, h := range has {
		if h {
			return i
		}
	}
	return -1
}

// clear removes a commit's rows from d's tables, for a re-derivation — on
// tx, together with the apply that replaces them.
func (d *deriver) clear(ctx context.Context, tx *sql.Tx, hash string) error {
	for _, stmt := range []string{
		`DELETE FROM ` + d.t.changes + ` WHERE commit_hash = ?`,
		`DELETE FROM ` + d.t.links + ` WHERE commit_hash = ?`,
		`DELETE FROM ` + d.t.fp + ` WHERE commit_hash = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, hash); err != nil {
			return fmt.Errorf("path changes: clear: %w", err)
		}
	}
	return nil
}

// apply writes a prepared commit's first-parent depth, jump pointers,
// degradation mark and path_changes rows, on tx — SQL ONLY: a git read while
// applying is errReadInApply. A commit already derived is left as is. Every
// parent present in the object store must be derived already.
func (d *deriver) apply(ctx context.Context, tx *sql.Tx, pc *preparedCommit) error {
	d.applying = true
	defer func() { d.applying = false }()
	c := pc.c
	hash := c.Hash.String()
	if have, err := d.isDerived(ctx, tx, hash); err != nil || have {
		return err
	}
	degraded := pc.degraded
	depths := make([]int, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		if pc.parentAbsent[i] {
			depths[i] = -1
			continue
		}
		var pdeg int
		err := tx.QueryRowContext(ctx, `SELECT depth, degraded FROM `+d.t.fp+` WHERE commit_hash = ?`, p.String()).Scan(&depths[i], &pdeg)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s (parent of %s)", errParentUnderived, p, hash)
		}
		if err != nil {
			return fmt.Errorf("path changes: parent depth: %w", err)
		}
		if pdeg == degradedStructural {
			degraded = degradedStructural // a cut depth below cuts it here too
		}
	}

	// First-parent depth and jump pointers: up[0] is the first parent,
	// up[k] = up[k-1] of up[k-1].
	depth := 0
	ups := []byte{} // a root (or a boundary) has none; the column is NOT NULL
	if len(c.ParentHashes) > 0 && depths[0] >= 0 {
		depth = depths[0] + 1
		anc := c.ParentHashes[0]
		for level := 0; ; level++ {
			ups = append(ups, anc[:]...)
			next, ok, err := jumpFrom(ctx, tx, d.t, anc.String(), level)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			anc = next
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO `+d.t.fp+` (commit_hash, depth, ups, degraded) VALUES (?, ?, ?, ?)`, hash, depth, ups, degraded); err != nil {
		return fmt.Errorf("path changes: mark: %w", err)
	}
	msg := firstLine(c.Message)
	for _, pp := range pc.paths {
		if err := d.applyPath(ctx, tx, c, hash, depth, depths, msg, pp); err != nil {
			return err
		}
	}
	return nil
}

func (d *deriver) applyPath(ctx context.Context, tx *sql.Tx, c *object.Commit, hash string, depth int, parentDepths []int, msg string, pp preparedPath) error {
	path := pp.path
	if pp.sameAs >= 0 {
		// A carry: the commit took this content from parent sameAs.
		from, _, err := liveChange(ctx, tx, d.t, path, c.ParentHashes[pp.sameAs].String(), 0)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO `+d.t.changes+` (path, commit_hash, entry, resolves_to, blob, message, fp_depth)
			VALUES (?, ?, 0, ?, ?, ?, ?)`, path, hash, from, pp.to.String(), msg, depth)
		return err
	}

	author := c.Author.When.Unix()
	orderAt, gen, action := author, 0, "added"
	for i := range c.ParentHashes {
		if !pp.parentHas[i] || parentDepths[i] < 0 {
			continue
		}
		action = "modified"
		from, _, err := liveChange(ctx, tx, d.t, path, c.ParentHashes[i].String(), 0)
		if err != nil {
			return err
		}
		if from != "" {
			var fo int64
			var fg int
			err := tx.QueryRowContext(ctx, `SELECT order_at, gen FROM `+d.t.changes+` WHERE path = ? AND commit_hash = ?`, path, from).Scan(&fo, &fg)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("path changes: link key: %w", err)
			}
			orderAt = max(orderAt, fo)
			gen = max(gen, fg+1)
		}
		fromBlob := ""
		if !pp.blobs[i].IsZero() {
			fromBlob = pp.blobs[i].String()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO `+d.t.links+` (path, commit_hash, parent_order, from_commit, from_blob)
			VALUES (?, ?, ?, ?, ?)`, path, hash, i, from, fromBlob); err != nil {
			return fmt.Errorf("path changes: link: %w", err)
		}
	}
	blob := ""
	if !pp.to.IsZero() {
		blob = pp.to.String()
	}
	unavailable := 0
	diff := pp.diff
	if pp.unavailable {
		unavailable, diff = 1, ""
	}
	_, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO `+d.t.changes+` (path, commit_hash, entry, resolves_to, blob, action, author_at, order_at, gen, message, diff, fp_depth, content_unavailable)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, path, hash, hash, blob, action, author, orderAt, gen, msg, diff, depth, unavailable)
	return err
}

// hook is the CommitLogApply Derive hook for items whose hashes are hashes:
// apply the prepared commit (SQL only), then — whether it was prepared here or
// derived earlier — refuse to record a commit that is not derived.
func (d *deriver) hook(hashes []string, prepared []*preparedCommit) func(ctx context.Context, tx *sql.Tx, i int) error {
	return func(ctx context.Context, tx *sql.Tx, i int) error {
		if prepared != nil && prepared[i] != nil {
			if err := d.apply(ctx, tx, prepared[i]); err != nil {
				return err
			}
		}
		derived, err := d.isDerived(ctx, tx, hashes[i])
		if err != nil {
			return err
		}
		if !derived {
			return fmt.Errorf("%w: %s", errNotDerived, hashes[i])
		}
		return nil
	}
}

// liveChange returns the change of path live at commit X in table set t: the
// nearest commit on X's first-parent line (X included) with a row, as the
// entry it resolves to and the row's own commit. With branchID != 0 only rows
// visible on that branch count. "" when there is none.
//
// No walk: candidate rows are scanned newest first by first-parent depth, in
// keyset pages, and "is R on X's first-parent line" is answered with the
// commit_fp jump pointers in O(log depth) lookups. X must be derived.
func liveChange(ctx context.Context, q storegit.CtxExecer, t pcTables, path, x string, branchID int64) (resolvesTo, row string, err error) {
	var xDepth int
	err = q.QueryRowContext(ctx, `SELECT depth FROM `+t.fp+` WHERE commit_hash = ?`, x).Scan(&xDepth)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("live change: depth: %w", err)
	}
	join := ""
	if branchID != 0 {
		join = ` JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = ` + fmt.Sprint(branchID)
	}
	query := `SELECT pc.commit_hash, pc.resolves_to, pc.fp_depth FROM ` + t.changes + ` pc` + join + `
		 WHERE pc.path = ? AND (pc.fp_depth < ? OR (pc.fp_depth = ? AND pc.commit_hash < ?))
		 ORDER BY pc.fp_depth DESC, pc.commit_hash DESC LIMIT 16`
	type cand struct {
		commit, resolves string
		depth            int
	}
	afterDepth, afterCommit := xDepth, "\xff" // first page: everything at or below X
	for {
		rows, err := q.QueryContext(ctx, query, path, afterDepth, afterDepth, afterCommit)
		if err != nil {
			return "", "", fmt.Errorf("live change: %w", err)
		}
		var cands []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.commit, &c.resolves, &c.depth); err != nil {
				rows.Close()
				return "", "", err
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", "", err
		}
		for _, c := range cands {
			ok, err := onFirstParentLine(ctx, q, t, c.commit, c.depth, x, xDepth)
			if err != nil {
				return "", "", err
			}
			if ok {
				return c.resolves, c.commit, nil
			}
		}
		if len(cands) < 16 {
			return "", "", nil
		}
		last := cands[len(cands)-1]
		afterDepth, afterCommit = last.depth, last.commit
	}
}

// onFirstParentLine reports whether r (at depth rDepth) is x or one of x's
// first-parent ancestors, by jumping x down xDepth-rDepth steps.
func onFirstParentLine(ctx context.Context, q storegit.CtxExecer, t pcTables, r string, rDepth int, x string, xDepth int) (bool, error) {
	steps := xDepth - rDepth
	for level := 0; steps > 0; level++ {
		if steps&1 == 1 {
			next, ok, err := jumpFrom(ctx, q, t, x, level)
			if err != nil || !ok {
				return false, err
			}
			x = next.String()
		}
		steps >>= 1
	}
	return x == r, nil
}

// jumpFrom returns the commit 2^level first-parent steps below commit, from
// its jump pointers. ok is false past the root (or a boundary).
func jumpFrom(ctx context.Context, q storegit.CtxExecer, t pcTables, commit string, level int) (plumbing.Hash, bool, error) {
	var ups []byte
	err := q.QueryRowContext(ctx, `SELECT ups FROM `+t.fp+` WHERE commit_hash = ?`, commit).Scan(&ups)
	if errors.Is(err, sql.ErrNoRows) {
		return plumbing.ZeroHash, false, nil
	}
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("first-parent jump: %w", err)
	}
	off := level * len(plumbing.ZeroHash)
	if off+len(plumbing.ZeroHash) > len(ups) {
		return plumbing.ZeroHash, false, nil
	}
	var h plumbing.Hash
	copy(h[:], ups[off:])
	return h, true, nil
}

// LiveRevision returns the commit whose version of path is live at anchor on
// branch: the nearest commit on the anchor's first-parent line (anchor
// included) that added or modified path, counting only commits visible on the
// branch. It is RevisionsBefore(anchor, 1) — path_changes has a row for every
// commit_log add/modify row of a .md path — answered with jump pointers
// instead of a first-parent walk; below a history boundary (a missing commit
// object) it finds nothing. "" when there is none.
func (hq *historyQuery) LiveRevision(ctx context.Context, branch, path, anchor string) (string, error) {
	branchID, err := hq.rh.branchID(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("LiveRevision: branchID: %w", err)
	}
	_, row, err := liveChange(ctx, conn(ctx, hq.rh.db), activeTables, path, anchor, branchID)
	return row, err
}

// FactRevision is one change of a path's content: the commit that introduced a
// blob none of its parents had.
type FactRevision struct {
	Commit     string
	Blob       string // "" when content_unavailable left it unknown
	Action     string // "added" | "modified"
	Message    string // first line of the commit message
	AuthoredAt int64  // Unix seconds, the commit's author date
	// FromBlob is the content this change was edited from: the path's blob at
	// the first parent that has it. "" for an addition, or when unknown.
	FromBlob string
	// Diff is the change against FromBlob; nil when there is none to show (an
	// addition, no tracked change, content that is not a fact, or content
	// that is unavailable).
	Diff *RevisionDiff
	// ContentUnavailable: this revision's content, or the content it was
	// edited from, is missing from the repository.
	ContentUnavailable bool
}

// PathHistoryCursor is a keyset position in a path history: the changes
// still to be listed next (the frontier of the link walk), plus whether the
// anchor was on the branch when the history started.
type PathHistoryCursor struct {
	Frontier       []string
	AnchorOnBranch bool
}

// PathHistory returns up to limit changes of path, newest first, in the
// history of anchorCommit on branch, and the position to continue from (nil
// when the history is exhausted). cur == nil starts at the change live at the
// anchor, which is always the first one returned. Reads only: every indexed
// commit was derived before it was indexed.
//
// Order: newest first by author date, but a change is never listed below one
// it descends from (the precomputed order_at/gen key). ErrHistoryChanged
// reports that a continuation's anchor left the branch, or that a change the
// walk needs (the frontier, or a link from it) is not visible on the branch.
func (hq *historyQuery) PathHistory(ctx context.Context, branch, path, anchorCommit string, cur *PathHistoryCursor, limit int) ([]FactRevision, *PathHistoryCursor, error) {
	if anchorCommit == "" || limit <= 0 {
		return nil, nil, nil
	}
	branchID, err := hq.rh.branchID(ctx, branch)
	if err != nil {
		return nil, nil, fmt.Errorf("PathHistory: branchID: %w", err)
	}
	db := conn(ctx, hq.rh.db)
	onBranch := func(h string) (bool, error) {
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`, branchID, h).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}

	h := &changeHeap{}
	seen := map[string]bool{}
	next := &PathHistoryCursor{}
	if cur == nil {
		anchorOn, err := onBranch(anchorCommit)
		if err != nil {
			return nil, nil, err
		}
		next.AnchorOnBranch = anchorOn
		if anchorOn {
			// Indexing derives every commit it records, so an indexed but
			// underived anchor is a broken invariant (only damage makes it):
			// say so rather than answer with an empty history.
			var marked int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+activeTables.fp+` WHERE commit_hash = ?`, anchorCommit).Scan(&marked); err != nil {
				return nil, nil, fmt.Errorf("PathHistory: mark: %w", err)
			}
			if marked == 0 {
				return nil, nil, fmt.Errorf("PathHistory: commit %s is indexed on %s but its history was never derived", anchorCommit, branch)
			}
		}
		// Candidate rows are always scoped to the branch: rows of commits on
		// no branch (never garbage-collected) are not even scanned.
		head, _, err := liveChange(ctx, db, activeTables, path, anchorCommit, branchID)
		if err != nil || head == "" {
			return nil, nil, err
		}
		cur = &PathHistoryCursor{Frontier: []string{head}}
	} else {
		next.AnchorOnBranch = cur.AnchorOnBranch
		if cur.AnchorOnBranch {
			on, err := onBranch(anchorCommit)
			if err != nil {
				return nil, nil, err
			}
			if !on {
				return nil, nil, ErrHistoryChanged
			}
		}
	}
	for _, f := range cur.Frontier {
		if seen[f] {
			continue
		}
		n, ok, err := loadChange(ctx, db, branchID, path, f)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, ErrHistoryChanged
		}
		seen[f] = true
		heap.Push(h, n)
	}

	var out []FactRevision
	for h.Len() > 0 && len(out) < limit {
		n := heap.Pop(h).(changeNode)
		out = append(out, n.FactRevision)
		for _, from := range n.links {
			if from == "" || seen[from] {
				continue
			}
			m, ok, err := loadChange(ctx, db, branchID, path, from)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				// A change's ancestors are on every branch it is on; a link
				// that is not visible means the branch moved under us.
				return nil, nil, ErrHistoryChanged
			}
			seen[from] = true
			heap.Push(h, m)
		}
	}
	if h.Len() == 0 {
		return out, nil, nil
	}
	for _, n := range *h {
		next.Frontier = append(next.Frontier, n.Commit)
	}
	sort.Strings(next.Frontier)
	return out, next, nil
}

// changeNode is a change plus its sort key and edited-from links.
type changeNode struct {
	FactRevision
	orderAt int64
	gen     int
	links   []string
}

// loadChange reads one entry and its links. ok is false when the entry is not
// indexed or not visible on the branch.
func loadChange(ctx context.Context, db storegit.CtxExecer, branchID int64, path, commit string) (changeNode, bool, error) {
	n := changeNode{FactRevision: FactRevision{Commit: commit}}
	var diff string
	var unavailable int
	err := db.QueryRowContext(ctx, `
		SELECT pc.blob, pc.action, pc.author_at, pc.order_at, pc.gen, pc.message, pc.diff, pc.content_unavailable
		  FROM `+activeTables.changes+` pc
		  JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = ?
		 WHERE pc.path = ? AND pc.commit_hash = ? AND pc.entry = 1`, branchID, path, commit).
		Scan(&n.Blob, &n.Action, &n.AuthoredAt, &n.orderAt, &n.gen, &n.Message, &diff, &unavailable)
	if errors.Is(err, sql.ErrNoRows) {
		return n, false, nil
	}
	if err != nil {
		return n, false, fmt.Errorf("PathHistory: change %s: %w", commit, err)
	}
	n.ContentUnavailable = unavailable != 0
	if diff != "" {
		n.Diff = &RevisionDiff{}
		if err := json.Unmarshal([]byte(diff), n.Diff); err != nil {
			return n, false, fmt.Errorf("PathHistory: diff of %s: %w", commit, err)
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT from_commit, from_blob FROM `+activeTables.links+`
		 WHERE path = ? AND commit_hash = ? ORDER BY parent_order`, path, commit)
	if err != nil {
		return n, false, fmt.Errorf("PathHistory: links %s: %w", commit, err)
	}
	defer rows.Close()
	for rows.Next() {
		var from, fromBlob string
		if err := rows.Scan(&from, &fromBlob); err != nil {
			return n, false, err
		}
		if n.FromBlob == "" {
			n.FromBlob = fromBlob
		}
		n.links = append(n.links, from)
	}
	return n, true, rows.Err()
}

// changeHeap pops the newest change first: order_at desc, gen desc, commit asc.
// Because a change's key is strictly after every change it was edited from,
// popping from the live change yields the whole history in key order.
type changeHeap []changeNode

func (h changeHeap) Len() int { return len(h) }
func (h changeHeap) Less(i, j int) bool {
	if h[i].orderAt != h[j].orderAt {
		return h[i].orderAt > h[j].orderAt
	}
	if h[i].gen != h[j].gen {
		return h[i].gen > h[j].gen
	}
	return h[i].Commit < h[j].Commit
}
func (h changeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *changeHeap) Push(x any)   { *h = append(*h, x.(changeNode)) }
func (h *changeHeap) Pop() any {
	old := *h
	n := old[len(old)-1]
	*h = old[:len(old)-1]
	return n
}
