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
	"time"

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
// edited from. See kb/decisions/mcp/explain/history-enumeration.
//
// WHEN rows are written: in the transaction that records a commit in
// branch_commits (CommitLogApply's Derive hook, see populateCommitLog and
// AppendCommitLog), parents before children. Everything read from git objects
// (blobs, diffs) is PREPARED before that transaction opens; inside it only
// SQL runs (apply). An indexed commit is therefore
// never visible without its rows, and knomit_explain never derives anything.
// A database that predates the tables, or a pathChangesVersion bump, is
// derived in one batched pass (deriveUnderived) at the start of the first
// populate — while the repo opens, before it serves a request.
//
// Every row is content-addressed: keyed by commit and computed only from git
// objects and the rows of the commit's ancestors, which never change for a
// given hash. Branch visibility is applied at read time.

// pathChangesVersion is the version of the derivation below. A mismatch with
// the stored meta value empties the tables so deriveUnderived recomputes them.
const pathChangesVersion = "1"

const pathChangesVersionKey = "path_changes_version"

// pathChangeBatch is how many commits one transaction derives during the
// one-time pass and bulk population: one commit per transaction would cost
// one WAL commit each.
const pathChangeBatch = 1000

// ErrHistoryChanged is returned for a PathHistory continuation whose anchor
// has left the branch, or whose position names a change no longer on the
// branch: the history it was paging is gone, and the caller must restart.
var ErrHistoryChanged = errors.New("path history changed since this position was issued")

// errParentUnderived is returned when a commit is offered for derivation
// before one of its parents. Deriving it anyway would record an unresolved
// link, and the history below it would stay truncated.
var errParentUnderived = errors.New("parent commit is not derived yet")

// deriveUnderived derives every indexed commit (any branch) that has no
// commit_fp mark — the tables' first fill on an upgraded database, or their
// refill after a pathChangesVersion bump — parents first, in batched
// transactions. A no-op costs one version lookup and one existence probe.
func (rh *repoHandler) deriveUnderived(ctx context.Context) error {
	if rh.repo == nil {
		return nil
	}
	q := conn(ctx, rh.db)
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, pathChangesVersionKey).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("path changes: version: %w", err)
	}
	if v != pathChangesVersion {
		for _, stmt := range []string{
			`DELETE FROM path_changes`, `DELETE FROM path_change_links`,
			`DELETE FROM commit_fp`,
		} {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("path changes: reset: %w", err)
			}
		}
		if _, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, pathChangesVersionKey, pathChangesVersion); err != nil {
			return fmt.Errorf("path changes: version: %w", err)
		}
	}

	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT bc.commit_hash FROM branch_commits bc
		 WHERE NOT EXISTS (SELECT 1 FROM commit_fp f WHERE f.commit_hash = bc.commit_hash)`)
	if err != nil {
		return fmt.Errorf("path changes: pending: %w", err)
	}
	var hashes []plumbing.Hash
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, plumbing.NewHash(h))
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(hashes) == 0 {
		return err
	}

	start := time.Now()
	commits := make([]*object.Commit, 0, len(hashes))
	for _, h := range hashes {
		c, err := rh.repo.CommitObject(h)
		if err != nil {
			return fmt.Errorf("path changes: commit %s: %w", h, err)
		}
		commits = append(commits, c)
	}
	order := parentsFirst(commits)
	d := newDeriver(rh)
	for i := 0; i < len(order); i += pathChangeBatch {
		chunk := order[i:min(i+pathChangeBatch, len(order))]
		// Prepare (object reads, diffs) outside any transaction; apply (SQL
		// only) in one transaction per chunk. See CommitLogApply on why.
		prepared := make([]*preparedCommit, len(chunk))
		for j, c := range chunk {
			paths, err := changedMarkdownPaths(ctx, q, c.Hash.String())
			if err != nil {
				return err
			}
			if prepared[j], err = d.prepare(c, paths); err != nil {
				return err
			}
		}
		bctx, tx, own, err := beginTxIfNeeded(ctx, rh.db)
		if err != nil {
			return fmt.Errorf("path changes: begin: %w", err)
		}
		for _, p := range prepared {
			if err := d.apply(bctx, tx, p); err != nil {
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
	log.Info().Int("commits", len(order)).Dur("elapsed", time.Since(start)).Msg("path changes: derived")
	return nil
}

// changedMarkdownPaths returns the .md paths commit_log records as added or
// modified in hash (against its first parent).
func changedMarkdownPaths(ctx context.Context, q storegit.CtxExecer, hash string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT path FROM commit_log WHERE commit_hash = ? AND action IN ('added','modified') AND path LIKE '%.md'`, hash)
	if err != nil {
		return nil, fmt.Errorf("path changes: changed paths: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// markdownChanges picks the .md add/modify paths out of a commit's
// commit_log entries, exactly as commit_log stores them: one row per
// (commit, path), the FIRST entry winning (INSERT OR IGNORE) — a case-only
// rename yields a delete and an add of the same lowercased path.
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

// deriver derives commits into path_changes. It keeps git object caches for
// the life of one populate or one-time pass; every SQL statement runs on the
// transaction it is handed, so the rows of parents derived earlier in the
// same transaction are visible.
type deriver struct {
	rh      *repoHandler
	trees   map[plumbing.Hash]*object.Tree
	commits map[plumbing.Hash]*object.Commit
	facts   map[plumbing.Hash]*fact.Fact // parsed blobs; nil when unparseable
}

func newDeriver(rh *repoHandler) *deriver {
	return &deriver{rh: rh, trees: map[plumbing.Hash]*object.Tree{}, commits: map[plumbing.Hash]*object.Commit{}, facts: map[plumbing.Hash]*fact.Fact{}}
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

// parsed returns the fact in a blob, parsed once per deriver; nil when the
// blob cannot be read or is not a fact.
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

// preparedCommit is everything deriving a commit needs from git objects —
// read before the indexing transaction opens. apply turns it into rows with
// SQL only.
type preparedCommit struct {
	c     *object.Commit
	paths []preparedPath
}

type preparedPath struct {
	path   string
	to     plumbing.Hash
	blobs  []plumbing.Hash // per parent; zero when the parent lacks the path
	sameAs int             // the first parent whose blob equals to, or -1
	diff   string          // RevisionDiff JSON against the first parent that has the path
}

// prepare reads, for each changed .md path of c, its blob at c and at every
// parent (case-insensitively), and computes the diff an entry would store.
// Git object reads only; no SQL.
func (d *deriver) prepare(c *object.Commit, paths []string) (*preparedCommit, error) {
	pc := &preparedCommit{c: c}
	for _, path := range paths {
		to, err := d.blobAt(c.Hash, path)
		if err != nil {
			return nil, err
		}
		if to.IsZero() {
			continue
		}
		pp := preparedPath{path: path, to: to, blobs: make([]plumbing.Hash, len(c.ParentHashes)), sameAs: -1}
		for i, p := range c.ParentHashes {
			if _, err := d.commit(p); err != nil {
				continue // not in the object store (a shallow boundary)
			}
			if pp.blobs[i], err = d.blobAt(p, path); err != nil {
				return nil, err
			}
			if pp.blobs[i] == to && pp.sameAs < 0 {
				// Content carried over from parent i. i == 0 is a case-only
				// rename (commit_log records it; the blob is unchanged): still
				// a row, so rows stay one-to-one with commit_log's add/modify
				// rows.
				pp.sameAs = i
			}
		}
		if pp.sameAs < 0 {
			for _, b := range pp.blobs {
				if b.IsZero() {
					continue
				}
				// The first parent that has the path is the content this
				// change was edited from: its diff base.
				if cur := d.parsed(path, to); cur != nil {
					if rd := revisionDelta(d.parsed(path, b), *cur); rd != nil {
						js, _ := json.Marshal(rd)
						pp.diff = string(js)
					}
				}
				break
			}
		}
		pc.paths = append(pc.paths, pp)
	}
	return pc, nil
}

// apply writes a prepared commit's first-parent depth and jump pointers and
// its path_changes rows, on tx — SQL only. A commit already derived
// (content-addressed, e.g. through another branch) is left as is. Every parent
// in the object store must be derived first; otherwise errParentUnderived.
func (d *deriver) apply(ctx context.Context, tx *sql.Tx, pc *preparedCommit) error {
	c := pc.c
	hash := c.Hash.String()
	var have int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM commit_fp WHERE commit_hash = ?`, hash).Scan(&have); err != nil {
		return fmt.Errorf("path changes: mark: %w", err)
	}
	if have > 0 {
		return nil
	}
	depths := make([]int, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		err := tx.QueryRowContext(ctx, `SELECT depth FROM commit_fp WHERE commit_hash = ?`, p.String()).Scan(&depths[i])
		if errors.Is(err, sql.ErrNoRows) {
			if _, oerr := d.commit(p); oerr != nil {
				depths[i] = -1 // not in the object store (a shallow boundary): no history below
				continue
			}
			return fmt.Errorf("%w: %s (parent of %s)", errParentUnderived, p, hash)
		}
		if err != nil {
			return fmt.Errorf("path changes: parent depth: %w", err)
		}
	}

	// First-parent depth and jump pointers: up[0] is the first parent,
	// up[k] = up[k-1] of up[k-1].
	depth := 0
	ups := []byte{} // a root has none; the column is NOT NULL
	if len(c.ParentHashes) > 0 && depths[0] >= 0 {
		depth = depths[0] + 1
		anc := c.ParentHashes[0]
		for level := 0; ; level++ {
			ups = append(ups, anc[:]...)
			next, ok, err := jumpFrom(ctx, tx, anc.String(), level)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			anc = next
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO commit_fp (commit_hash, depth, ups) VALUES (?, ?, ?)`, hash, depth, ups); err != nil {
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
	// A re-derivation (:rebuild) replaces the rows it writes.
	if _, err := tx.ExecContext(ctx, `DELETE FROM path_change_links WHERE path = ? AND commit_hash = ?`, path, hash); err != nil {
		return fmt.Errorf("path changes: clear links: %w", err)
	}
	if pp.sameAs >= 0 {
		// A carry: the commit took this content from parent sameAs.
		from, _, err := liveChange(ctx, tx, path, c.ParentHashes[pp.sameAs].String(), 0)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO path_changes (path, commit_hash, entry, resolves_to, blob, message, fp_depth)
			VALUES (?, ?, 0, ?, ?, ?, ?)`, path, hash, from, pp.to.String(), msg, depth)
		return err
	}

	author := c.Author.When.Unix()
	orderAt, gen, action := author, 0, "added"
	for i, b := range pp.blobs {
		if b.IsZero() || parentDepths[i] < 0 {
			continue
		}
		action = "modified"
		from, _, err := liveChange(ctx, tx, path, c.ParentHashes[i].String(), 0)
		if err != nil {
			return err
		}
		if from != "" {
			var fo int64
			var fg int
			err := tx.QueryRowContext(ctx, `SELECT order_at, gen FROM path_changes WHERE path = ? AND commit_hash = ?`, path, from).Scan(&fo, &fg)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("path changes: link key: %w", err)
			}
			orderAt = max(orderAt, fo)
			gen = max(gen, fg+1)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO path_change_links (path, commit_hash, parent_order, from_commit, from_blob)
			VALUES (?, ?, ?, ?, ?)`, path, hash, i, from, b.String()); err != nil {
			return fmt.Errorf("path changes: link: %w", err)
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO path_changes (path, commit_hash, entry, resolves_to, blob, action, author_at, order_at, gen, message, diff, fp_depth)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, path, hash, hash, pp.to.String(), action, author, orderAt, gen, msg, pp.diff, depth)
	return err
}

// liveChange returns the change of path live at commit X: the nearest commit
// on X's first-parent line (X included) with a path_changes row, as the entry
// it resolves to and the row's own commit. With branchID != 0 only rows
// visible on that branch count. "" when there is none.
//
// No walk: candidate rows are scanned newest first by first-parent depth, and
// "is R on X's first-parent line" is answered with commit_fp jump pointers
// in O(log depth) lookups. X must be derived (commit_fp).
func liveChange(ctx context.Context, q storegit.CtxExecer, path, x string, branchID int64) (resolvesTo, row string, err error) {
	var xDepth int
	err = q.QueryRowContext(ctx, `SELECT depth FROM commit_fp WHERE commit_hash = ?`, x).Scan(&xDepth)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("live change: depth: %w", err)
	}
	// Candidates newest first, in small keyset pages: on a first-parent line
	// the first candidate is almost always the answer, and a path changed on
	// every commit has thousands of rows below X.
	join := ""
	if branchID != 0 {
		join = ` JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = ` + fmt.Sprint(branchID)
	}
	query := `SELECT pc.commit_hash, pc.resolves_to, pc.fp_depth FROM path_changes pc` + join + `
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
			ok, err := onFirstParentLine(ctx, q, c.commit, c.depth, x, xDepth)
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
func onFirstParentLine(ctx context.Context, q storegit.CtxExecer, r string, rDepth int, x string, xDepth int) (bool, error) {
	steps := xDepth - rDepth
	for level := 0; steps > 0; level++ {
		if steps&1 == 1 {
			next, ok, err := jumpFrom(ctx, q, x, level)
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
// its commit_fp jump pointers. ok is false past the root.
func jumpFrom(ctx context.Context, q storegit.CtxExecer, commit string, level int) (plumbing.Hash, bool, error) {
	var ups []byte
	err := q.QueryRowContext(ctx, `SELECT ups FROM commit_fp WHERE commit_hash = ?`, commit).Scan(&ups)
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
// instead of a first-parent walk. "" when there is none.
func (hq *historyQuery) LiveRevision(ctx context.Context, branch, path, anchor string) (string, error) {
	branchID, err := hq.rh.branchID(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("LiveRevision: branchID: %w", err)
	}
	_, row, err := liveChange(ctx, conn(ctx, hq.rh.db), path, anchor, branchID)
	return row, err
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
// anchor, which is always the first one returned. Reads only: every indexed
// commit was derived in the transaction that indexed it.
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
		// On-branch anchor: every first-parent ancestor is on the branch, so
		// no per-row branch filter is needed.
		scope := branchID
		if anchorOn {
			scope = 0
			// Indexing derives every commit it records, so an indexed but
			// underived anchor is a broken invariant (e.g. a failed open-time
			// pass): say so rather than answer with an empty history.
			var marked int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commit_fp WHERE commit_hash = ?`, anchorCommit).Scan(&marked); err != nil {
				return nil, nil, fmt.Errorf("PathHistory: mark: %w", err)
			}
			if marked == 0 {
				return nil, nil, fmt.Errorf("PathHistory: commit %s is indexed on %s but its history was never derived", anchorCommit, branch)
			}
		}
		head, _, err := liveChange(ctx, db, path, anchorCommit, scope)
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
