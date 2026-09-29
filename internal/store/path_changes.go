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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	storegit "knomit/internal/store/git"
)

// Per-path change history, precomputed (tables: migration 000032).
//
// A path's history is the set of ENTRIES — commits that introduced a blob for
// the path that none of their parents had — linked to the entries they were
// edited from. It is derived once per commit, when the commit is indexed, so
// knomit_explain reads a page with indexed lookups and never walks commits.
// See kb/decisions/mcp/explain/history-enumeration.
//
// Every row is content-addressed: keyed by commit and computed only from git
// objects and the rows of the commit's ancestors, which never change for a
// given hash. Rewinds, purges and commit_log rebuilds therefore neither touch
// nor invalidate these tables; a change to the derivation bumps
// pathChangesVersion instead.

// pathChangesVersion is the version of the derivation below. A mismatch with
// the stored meta value empties the tables so syncPathChanges recomputes them.
const pathChangesVersion = "1"

const pathChangesVersionKey = "path_changes_version"

// pathChangeBatch is how many commits one derivation transaction covers.
const pathChangeBatch = 256

// ErrHistoryChanged is returned for a PathHistory continuation whose anchor
// has left the branch, or whose position names a change no longer on the
// branch or no longer indexed: the history it was paging is gone, and the
// caller must restart from the first page.
var ErrHistoryChanged = errors.New("path history changed since this position was issued")

// ensurePathChangesVersion empties the derived tables when they were built by
// another version of the derivation.
func (rh *repoHandler) ensurePathChangesVersion(ctx context.Context) error {
	var v string
	err := rh.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, pathChangesVersionKey).Scan(&v)
	if err == nil && v == pathChangesVersion {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("path changes version: %w", err)
	}
	for _, q := range []string{
		`DELETE FROM path_changes`,
		`DELETE FROM path_change_links`,
		`DELETE FROM path_change_commits`,
	} {
		if _, err := rh.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("path changes reset: %w", err)
		}
	}
	_, err = rh.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, pathChangesVersionKey, pathChangesVersion)
	return err
}

// syncPathChanges derives path_changes for every indexed commit (any branch)
// that has not been derived yet, parents before children. Idempotent and
// cheap when nothing is pending.
func (rh *repoHandler) syncPathChanges(ctx context.Context) error {
	if rh.repo == nil || storegit.TxFromContext(ctx) != nil {
		// Under a caller's transaction (which holds the write lock) deriving
		// would wait on that lock; the next sync, or the next history read,
		// derives instead.
		return nil
	}
	if err := rh.ensurePathChangesVersion(ctx); err != nil {
		return err
	}
	rows, err := rh.db.QueryContext(ctx, `
		SELECT DISTINCT bc.commit_hash FROM branch_commits bc
		 WHERE NOT EXISTS (SELECT 1 FROM path_change_commits d WHERE d.commit_hash = bc.commit_hash)`)
	if err != nil {
		return fmt.Errorf("path changes: pending: %w", err)
	}
	pending := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		pending[h] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	order, err := rh.pendingTopoOrder(ctx, pending)
	if err != nil {
		return err
	}
	for start := 0; start < len(order); start += pathChangeBatch {
		if err := rh.derivePathChangesBatch(ctx, order[start:min(start+pathChangeBatch, len(order))]); err != nil {
			return err
		}
	}
	log.Debug().Int("commits", len(order)).Msg("path changes: derived")
	return nil
}

// syncPathChangesFor derives one newly indexed commit — the append path.
// When the commit is already derived it is a single lookup; when any of its
// indexed parents is not derived yet it falls back to the full sync.
func (rh *repoHandler) syncPathChangesFor(ctx context.Context, hash string) error {
	if rh.repo == nil || storegit.TxFromContext(ctx) != nil {
		return nil // see syncPathChanges
	}
	var done int
	if err := rh.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_change_commits WHERE commit_hash = ?`, hash).Scan(&done); err != nil {
		return fmt.Errorf("path changes: done: %w", err)
	}
	if done > 0 {
		var v string
		err := rh.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, pathChangesVersionKey).Scan(&v)
		if err == nil && v == pathChangesVersion {
			return nil
		}
	}
	if err := rh.ensurePathChangesVersion(ctx); err != nil {
		return err
	}
	var missing int
	if err := rh.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commit_parents cp
		  JOIN branch_commits bc ON bc.commit_hash = cp.parent_hash
		 WHERE cp.commit_hash = ?
		   AND NOT EXISTS (SELECT 1 FROM path_change_commits d WHERE d.commit_hash = cp.parent_hash)`, hash).Scan(&missing); err != nil {
		return fmt.Errorf("path changes: parents derived: %w", err)
	}
	if missing > 0 {
		return rh.syncPathChanges(ctx)
	}
	return rh.derivePathChangesBatch(ctx, []string{hash})
}

// pendingTopoOrder orders pending commits parents-first (Kahn), so every
// commit is derived after the ancestors its links resolve to. Parents outside
// the pending set are already derived or not indexed.
func (rh *repoHandler) pendingTopoOrder(ctx context.Context, pending map[string]bool) ([]string, error) {
	rows, err := rh.db.QueryContext(ctx, `SELECT commit_hash, parent_hash FROM commit_parents`)
	if err != nil {
		return nil, fmt.Errorf("path changes: parents: %w", err)
	}
	indeg := map[string]int{}
	children := map[string][]string{}
	for rows.Next() {
		var c, p string
		if err := rows.Scan(&c, &p); err != nil {
			rows.Close()
			return nil, err
		}
		if pending[c] && pending[p] {
			indeg[c]++
			children[p] = append(children[p], c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var ready []string
	for h := range pending {
		if indeg[h] == 0 {
			ready = append(ready, h)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(pending))
	for len(ready) > 0 {
		h := ready[0]
		ready = ready[1:]
		order = append(order, h)
		for _, c := range children[h] {
			indeg[c]--
			if indeg[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if len(order) != len(pending) {
		return nil, fmt.Errorf("path changes: commit graph has a cycle (%d of %d ordered)", len(order), len(pending))
	}
	return order, nil
}

func (rh *repoHandler) derivePathChangesBatch(ctx context.Context, commits []string) error {
	d, err := newDeriver(ctx, rh)
	if err != nil {
		return err
	}
	defer d.close()
	for _, h := range commits {
		if err := d.derive(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

// deriver holds one derivation run's prepared statements and object caches.
// The work per commit is bounded by the .md paths it changed: commit_log
// already names them (against the first parent), so only those paths' blobs
// are looked up — never a tree diff.
//
// LOCKING: every read (git objects, commit_log, ancestors' rows) and every
// diff happens OUTSIDE a transaction; a commit's rows are buffered and written
// in one short transaction of their own. The repo DB takes the write lock at
// BEGIN (_txlock=immediate), so holding a transaction across the reads would
// stall every other writer on every branch for the duration.
type deriver struct {
	rh       *repoHandler
	changed  *sql.Stmt
	cands    *sql.Stmt
	key      *sql.Stmt
	insEntry *sql.Stmt
	insCarry *sql.Stmt
	insLink  *sql.Stmt
	mark     *sql.Stmt
	nearest  *sql.Stmt
	pending  []pendingWrite
	trees    map[plumbing.Hash]*object.Tree
	commits  map[plumbing.Hash]*object.Commit
	facts    map[plumbing.Hash]*fact.Fact // parsed blobs; nil when unparseable
}

// pendingWrite is one buffered insert of the commit being derived.
type pendingWrite struct {
	st   *sql.Stmt
	args []any
}

func newDeriver(ctx context.Context, rh *repoHandler) (*deriver, error) {
	d := &deriver{rh: rh, trees: map[plumbing.Hash]*object.Tree{}, commits: map[plumbing.Hash]*object.Commit{}, facts: map[plumbing.Hash]*fact.Fact{}}
	for _, p := range []struct {
		dst **sql.Stmt
		sql string
	}{
		{&d.changed, `SELECT path FROM commit_log WHERE commit_hash = ? AND action IN ('added','modified') AND path LIKE '%.md'`},
		{&d.cands, `SELECT commit_hash FROM path_changes WHERE path = ? AND blob = ? AND entry = 1 LIMIT 2`},
		{&d.key, `SELECT order_at, gen FROM path_changes WHERE path = ? AND commit_hash = ?`},
		{&d.insEntry, `INSERT OR REPLACE INTO path_changes (path, commit_hash, entry, resolves_to, blob, action, author_at, order_at, gen, message, diff) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`},
		{&d.insCarry, `INSERT OR REPLACE INTO path_changes (path, commit_hash, entry, resolves_to, blob, message) VALUES (?, ?, 0, ?, ?, ?)`},
		{&d.insLink, `INSERT OR REPLACE INTO path_change_links (path, commit_hash, parent_order, from_commit, from_blob) VALUES (?, ?, ?, ?, ?)`},
		{&d.mark, `INSERT OR IGNORE INTO path_change_commits (commit_hash) VALUES (?)`},
		{&d.nearest, nearestChangeSQL(false)},
	} {
		st, err := rh.db.PrepareContext(ctx, p.sql)
		if err != nil {
			d.close()
			return nil, fmt.Errorf("path changes: prepare: %w", err)
		}
		*p.dst = st
	}
	return d, nil
}

func (d *deriver) close() {
	for _, st := range []*sql.Stmt{d.changed, d.cands, d.key, d.insEntry, d.insCarry, d.insLink, d.mark, d.nearest} {
		if st != nil {
			st.Close()
		}
	}
}

func (d *deriver) write(st *sql.Stmt, args ...any) {
	d.pending = append(d.pending, pendingWrite{st: st, args: args})
}

// flush writes the buffered rows of one commit, and its derived mark, in one
// short transaction.
func (d *deriver) flush(ctx context.Context) error {
	tx, err := d.rh.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("path changes: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, w := range d.pending {
		if _, err := tx.StmtContext(ctx, w.st).ExecContext(ctx, w.args...); err != nil {
			return fmt.Errorf("path changes: write: %w", err)
		}
	}
	d.pending = d.pending[:0]
	return tx.Commit()
}

func (d *deriver) commit(h plumbing.Hash) (*object.Commit, error) {
	if c, ok := d.commits[h]; ok {
		return c, nil
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

func (d *deriver) tree(h plumbing.Hash) (*object.Tree, error) {
	if t, ok := d.trees[h]; ok {
		return t, nil
	}
	t, err := d.rh.repo.TreeObject(h)
	if err != nil {
		return nil, err
	}
	if len(d.trees) > 16384 {
		clear(d.trees)
	}
	d.trees[h] = t
	return t, nil
}

// parsed returns the fact in a blob, parsed once per batch; nil when the blob
// cannot be read or is not a fact.
func (d *deriver) parsed(path string, h plumbing.Hash) *fact.Fact {
	if f, ok := d.facts[h]; ok {
		return f
	}
	var out *fact.Fact
	if b, err := d.rh.repo.BlobObject(h); err == nil {
		if r, err := b.Reader(); err == nil {
			raw, err := io.ReadAll(r)
			r.Close()
			if err == nil {
				if f, err := fact.ParseFact(path, string(raw)); err == nil {
					out = &f
				}
			}
		}
	}
	if len(d.facts) > 1024 {
		clear(d.facts)
	}
	d.facts[h] = out
	return out
}

// blobAt returns path's blob in commit's tree, matching each component
// case-insensitively (fact paths are lowercase-canonical; the tree may not
// be). Zero when absent.
func (d *deriver) blobAt(commit plumbing.Hash, path string) (plumbing.Hash, error) {
	c, err := d.commit(commit)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	root, err := d.tree(c.TreeHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	e, err := treeEntryInsensitiveVia(d.tree, root, path)
	if err != nil {
		return plumbing.ZeroHash, nil
	}
	return e.Hash, nil
}

// derive writes the path_changes rows of one commit: one per .md path whose
// blob differs from the first parent's.
func (d *deriver) derive(ctx context.Context, hash string) error {
	rows, err := d.changed.QueryContext(ctx, hash)
	if err != nil {
		return fmt.Errorf("path changes: changed paths: %w", err)
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(paths) > 0 {
		c, err := d.commit(plumbing.NewHash(hash))
		if err != nil {
			return err
		}
		for _, path := range paths {
			if err := d.derivePath(ctx, c, path); err != nil {
				return err
			}
		}
	}
	d.write(d.mark, hash)
	return d.flush(ctx)
}

func (d *deriver) derivePath(ctx context.Context, c *object.Commit, path string) error {
	hash := c.Hash.String()
	to, err := d.blobAt(c.Hash, path)
	if err != nil || to.IsZero() {
		return err
	}
	blobs := make([]plumbing.Hash, len(c.ParentHashes))
	sameAs := -1
	for i, p := range c.ParentHashes {
		if blobs[i], err = d.blobAt(p, path); err != nil {
			return err
		}
		if blobs[i] == to {
			if i == 0 {
				return nil // unchanged against the first parent (a case-only rename)
			}
			if sameAs < 0 {
				sameAs = i
			}
		}
	}
	msg := firstLine(c.Message)

	if sameAs >= 0 {
		// A carry: the merge took this content from parent sameAs.
		from, err := d.liveEntry(ctx, c.ParentHashes[sameAs].String(), path, to.String())
		if err != nil {
			return err
		}
		d.write(d.insCarry, path, hash, from, to.String(), msg)
		return nil
	}

	author := c.Author.When.Unix()
	orderAt, gen, action := author, 0, "added"
	diff := ""
	for i, b := range blobs {
		if b.IsZero() {
			continue
		}
		if action == "added" {
			// The first parent that has the path is the content this change
			// was edited from: its diff base.
			if cur := d.parsed(path, to); cur != nil {
				if rd := revisionDelta(d.parsed(path, b), *cur); rd != nil {
					js, _ := json.Marshal(rd)
					diff = string(js)
				}
			}
		}
		action = "modified"
		from, err := d.liveEntry(ctx, c.ParentHashes[i].String(), path, b.String())
		if err != nil {
			return err
		}
		if from != "" {
			var fo int64
			var fg int
			err := d.key.QueryRowContext(ctx, path, from).Scan(&fo, &fg)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("path changes: link key: %w", err)
			}
			orderAt = max(orderAt, fo)
			gen = max(gen, fg+1)
		}
		d.write(d.insLink, path, hash, i, from, b.String())
	}
	d.write(d.insEntry, path, hash, hash, to.String(), action, author, orderAt, gen, msg, diff)
	return nil
}

// liveEntry returns the entry that introduced the path's content as it stands
// at commit, whose blob for the path is blob. When exactly one entry ever
// introduced that blob, it is that one: the content's introducer is always in
// commit's ancestry, so any other would make two. Otherwise (a revert, a
// replayed commit) the nearest change on commit's first-parent line decides.
// "" when nothing is indexed for it.
func (d *deriver) liveEntry(ctx context.Context, commit, path, blob string) (string, error) {
	rows, err := d.cands.QueryContext(ctx, path, blob)
	if err != nil {
		return "", fmt.Errorf("live entry: %w", err)
	}
	var cands []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return "", err
		}
		cands = append(cands, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	var out string
	err = d.nearest.QueryRowContext(ctx, commit, path, commit, path).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("nearest change: %w", err)
	}
	return out, nil
}

// nearestChangeSQL walks a commit's first-parent line and yields resolves_to
// of the first commit with a path_changes row for the path — the entry live
// there. It stops at the first hit. Parameters: (commit, path, commit, path),
// or with branch scoping (commit, branchID, path, commit, branchID, path):
// only commits visible on that branch count.
func nearestChangeSQL(branchScoped bool) string {
	hit := `(SELECT pc.resolves_to FROM path_changes pc WHERE pc.path = ? AND pc.commit_hash = %s)`
	if branchScoped {
		hit = `(SELECT pc.resolves_to FROM path_changes pc JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = ? WHERE pc.path = ? AND pc.commit_hash = %s)`
	}
	return `
		WITH RECURSIVE w(h, hit) AS (
		    SELECT ?, ` + fmt.Sprintf(hit, "?") + `
		    UNION ALL
		    SELECT cp.parent_hash, ` + fmt.Sprintf(hit, "cp.parent_hash") + `
		      FROM w JOIN commit_parents cp ON cp.commit_hash = w.h AND cp.parent_order = 0
		     WHERE w.hit IS NULL
		)
		SELECT hit FROM w WHERE hit IS NOT NULL LIMIT 1`
}

// nearestChange is nearestChangeSQL(true) as a query: the entry live at
// commit, counting only commits visible on the branch. "" when none.
func nearestChange(ctx context.Context, q storegit.CtxExecer, commit, path string, branchID int64) (string, error) {
	var out string
	err := q.QueryRowContext(ctx, nearestChangeSQL(true), commit, branchID, path, commit, branchID, path).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("nearest change: %w", err)
	}
	return out, nil
}

// FactRevision is one change of a path's content: the commit that introduced a
// blob none of its parents had.
type FactRevision struct {
	Commit     string
	Blob       string
	Action     string // "added" | "modified"
	Message    string // first line of the commit message
	AuthoredAt int64  // Unix seconds, the commit's author date
	// FromBlob is the content this change was edited from: the path's blob at
	// the first parent that has it. "" for an addition.
	FromBlob string
	// Diff is the change against FromBlob; nil when there is none to show (an
	// addition, no tracked change, or content that is not a fact).
	Diff *RevisionDiff
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
// anchor, which is always the first one returned.
//
// Order: newest first by author date, but a change is never listed below one
// it descends from (the precomputed order_at/gen key; see path_changes). On a
// continuation, ErrHistoryChanged reports that the anchor left the branch or a
// frontier change is no longer on it or no longer indexed.
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
			// Self-heal: a commit whose derivation failed or has not run yet
			// would make the history start at an older change.
			if err := hq.rh.syncPathChangesFor(ctx, anchorCommit); err != nil {
				return nil, nil, err
			}
		}
		head, err := hq.liveAtAnchor(ctx, db, branchID, path, anchorCommit, anchorOn)
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
				continue // not indexed: the history ends there
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

// liveAtAnchor resolves the change live at the anchor on the branch. Fast
// path: the anchor's blob names exactly one on-branch entry. Otherwise the
// first-parent line from the anchor decides, counting only on-branch commits.
func (hq *historyQuery) liveAtAnchor(ctx context.Context, db storegit.CtxExecer, branchID int64, path, anchor string, anchorOn bool) (string, error) {
	if anchorOn {
		if co, err := hq.rh.repo.CommitObject(plumbing.NewHash(anchor)); err == nil {
			if tree, err := co.Tree(); err == nil {
				if e, err := treeEntryInsensitive(hq.rh.repo, tree, path); err == nil {
					rows, err := db.QueryContext(ctx, `
						SELECT pc.commit_hash FROM path_changes pc
						  JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = ?
						 WHERE pc.path = ? AND pc.blob = ? AND pc.entry = 1 LIMIT 2`, branchID, path, e.Hash.String())
					if err != nil {
						return "", fmt.Errorf("PathHistory: live: %w", err)
					}
					var cands []string
					for rows.Next() {
						var c string
						if err := rows.Scan(&c); err != nil {
							rows.Close()
							return "", err
						}
						cands = append(cands, c)
					}
					rows.Close()
					if len(cands) == 1 {
						return cands[0], nil
					}
				}
			}
		}
	}
	return nearestChange(ctx, db, anchor, path, branchID)
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
	err := db.QueryRowContext(ctx, `
		SELECT pc.blob, pc.action, pc.author_at, pc.order_at, pc.gen, pc.message, pc.diff
		  FROM path_changes pc
		  JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = ?
		 WHERE pc.path = ? AND pc.commit_hash = ? AND pc.entry = 1`, branchID, path, commit).
		Scan(&n.Blob, &n.Action, &n.AuthoredAt, &n.orderAt, &n.gen, &n.Message, &diff)
	if errors.Is(err, sql.ErrNoRows) {
		return n, false, nil
	}
	if err != nil {
		return n, false, fmt.Errorf("PathHistory: change %s: %w", commit, err)
	}
	if diff != "" {
		n.Diff = &RevisionDiff{}
		if err := json.Unmarshal([]byte(diff), n.Diff); err != nil {
			return n, false, fmt.Errorf("PathHistory: diff of %s: %w", commit, err)
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT from_commit, from_blob FROM path_change_links
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
