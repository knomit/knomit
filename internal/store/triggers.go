// Triggers (F07): the store half of the trigger dispatcher. Everything here is
// a git READ or a write to the dispatcher's OWN two tables (trigger_watermarks,
// trigger_fires, migration 000028). Nothing here writes a ref, an object or a
// fact, and nothing here reads a clock except to stamp fired_at for the
// operator. The dispatcher itself is internal/repos/triggers.go; this file is
// what it calls under Acquire.
package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
)

// TriggerFireRetention is how many fire-log rows a repo keeps: the newest
// 10,000. The first count-capped table in the store — fires come in bursts, so
// an age cap would not bound them. The log is operator cache, not the audit
// trail (the trail is the trace trailers in git).
const TriggerFireRetention = 10000

// Fire outcomes. `if-false` is COUNTED in the dispatcher's statistics and never
// written as a row, so the cap holds real fires. `run` marks the one summary
// row each dispatcher run writes.
const (
	TriggerOutcomeEmitted     = "emitted"
	TriggerOutcomeIfFalse     = "if-false"
	TriggerOutcomeIfError     = "if-error"
	TriggerOutcomeIfTimeout   = "if-timeout"
	TriggerOutcomeUnparseable = "unparseable"
	TriggerOutcomeRun         = "run"
)

// ErrNoOntologyAtCommit: the commit's tree holds no ontology file at any of the
// known paths. The dispatcher keeps its last good trigger set.
var ErrNoOntologyAtCommit = errors.New("no ontology file in the commit's tree")

// TriggerFire is one row of trigger_fires: a fire (or an error outcome) of one
// trigger on one path, or, with Trigger == "" and Outcome == "run", the run
// row that summarises one dispatcher run.
type TriggerFire struct {
	ID             int64  `json:"id"`
	Trigger        string `json:"trigger"`
	Branch         string `json:"branch"`
	Path           string `json:"path,omitempty"`
	Episode        string `json:"episode,omitempty"`
	Source         string `json:"source,omitempty"`
	Commit         string `json:"commit,omitempty"`
	Trace          string `json:"trace,omitempty"`
	Outcome        string `json:"outcome"`
	Error          string `json:"error,omitempty"`
	Nonlinear      bool   `json:"nonlinear,omitempty"`
	RangeFrom      string `json:"range_from"`
	RangeTo        string `json:"range_to"`
	Evaluated      int    `json:"evaluated,omitempty"`
	Paths          int    `json:"paths,omitempty"`
	Fires          int    `json:"fires,omitempty"`
	FiresNotLogged int    `json:"fires_not_logged,omitempty"`
	DurationMS     int64  `json:"duration_ms,omitempty"`
	DiffMS         int64  `json:"diff_ms,omitempty"`
	ChangeMS       int64  `json:"change_ms,omitempty"`
	FiredAt        int64  `json:"fired_at"`
}

// TriggerRun is what one dispatcher run hands to RecordTriggerRun: the
// coalesced range, its counts and the fire rows to log (newest paths last).
type TriggerRun struct {
	Branch     string
	RangeFrom  string
	RangeTo    string
	Nonlinear  bool
	Paths      int // diff rows in the range
	Evaluated  int // (trigger, path) evaluations across every trigger
	Fires      int // emitted, before the retention cap
	DurationMS int64
	DiffMS     int64
	ChangeMS   int64
	Rows       []TriggerFire
}

// TouchResult is the answer of the treesame toucher walk (Toucher).
type TouchResult struct {
	Commit plumbing.Hash
	// Found is false when the walk reached a commit with no parent without
	// finding a toucher — a scrubbed path after a rewind. The caller takes
	// the defined fallback (commit = the advance's head, author unknown).
	Found bool
	// MergeToucher marks the one case where a merge IS the toucher: none of
	// its parents carries the path's blob, so the merge itself produced it (a
	// textual three-way merge, e.g. by GitHub). knomit's own merges resolve
	// per file, one side wins, so they always have a same-blob parent.
	MergeToucher bool
	Steps        int
}

// CommitInfo is what the dispatcher reads from a commit to build `change`.
type CommitInfo struct {
	Hash        plumbing.Hash
	AuthorName  string
	AuthorEmail string
	Message     string
	Parents     []plumbing.Hash
}

// TriggerIndex is the store surface of the trigger dispatcher.
type TriggerIndex interface {
	// DiffFacts is the two-tree diff of the whole ontology root between two
	// commits with NO ancestry check (changes.go): the dispatcher's advance
	// may be a rewind replay, which ChangesUnder refuses.
	DiffFacts(ctx context.Context, from, to plumbing.Hash) ([]PathChange, error)
	// OntologyAtCommit reads the ontology file from the commit's own tree,
	// choosing the path exactly as the store does (the first of
	// fact.OntologyPathsNewestFirst that exists as a file), and returns its
	// path, blob hash and content.
	OntologyAtCommit(ctx context.Context, commit plumbing.Hash) (path, blob string, data []byte, err error)
	// Toucher finds the commit that introduced the blob path carries at head:
	// git's own history simplification, with no clock. See the method.
	Toucher(ctx context.Context, head plumbing.Hash, path string) (TouchResult, error)
	CommitInfo(ctx context.Context, hash plumbing.Hash) (CommitInfo, error)
	// CommitSignerOf returns the verified SSHSIG signer of a commit; the
	// fingerprint is set ONLY when the signature verifies over the payload.
	CommitSignerOf(ctx context.Context, hash plumbing.Hash) (CommitSigner, error)
	// BlobAt reads path's content at commit; ok is false when absent.
	BlobAt(ctx context.Context, commit plumbing.Hash, path string) (content string, ok bool, err error)
	IsAncestor(ctx context.Context, a, b plumbing.Hash) (bool, error)
	// VerifiedAnchor returns F09's anchor ref for upstream (ZeroHash when
	// none) and the in-memory set of commits below it, which is NIL until a
	// fold has run in this process (verify_gate.go cachedBelow). The
	// dispatcher falls back to AncestorSet once per run in that case.
	VerifiedAnchor(ctx context.Context, upstream string) (plumbing.Hash, map[plumbing.Hash]bool, error)
	// AncestorSet is every commit reachable from tip, tip included.
	AncestorSet(ctx context.Context, tip plumbing.Hash) (map[plumbing.Hash]bool, error)

	// TriggerWatermarks returns trigger name → commit hash for branch.
	TriggerWatermarks(ctx context.Context, branch string) (map[string]string, error)
	// RecordTriggerRun is tx1: the run's fire rows, capped at
	// TriggerFireRetention (the newest paths kept, the rest counted in the run
	// row's fires_not_logged), plus exactly ONE run row. Returns how many fire
	// rows were written.
	RecordTriggerRun(ctx context.Context, run TriggerRun) (int, error)
	// AdvanceTriggerWatermarks is tx2: upserts set, deletes del, then prunes
	// trigger_fires to the newest TriggerFireRetention rows.
	AdvanceTriggerWatermarks(ctx context.Context, branch string, set map[string]string, del []string) error
	// RecentTriggerFires returns the newest limit rows for branch, newest first.
	RecentTriggerFires(ctx context.Context, branch string, limit int) ([]TriggerFire, error)
}

// Triggers returns the trigger dispatcher's store surface.
func (s *Service) Triggers() TriggerIndex { return s.rh }

// Compile-time assertion: repoHandler must implement TriggerIndex.
var _ TriggerIndex = (*repoHandler)(nil)

// SignerFingerprint is the full pki.Fingerprint of the key this store signs
// authored commits with (its signer, or inside a test binary the test
// fallback), or "" when it has none. `change.source` is `local` exactly when
// the firing commit's verified signer carries this fingerprint.
func (s *Service) SignerFingerprint() string {
	signer, err := s.rh.commitSigner()
	if err != nil {
		return ""
	}
	cpk, ok := signer.PublicKey().(ssh.CryptoPublicKey)
	if !ok {
		return ""
	}
	ed, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return pki.Fingerprint(ed)
}

// OntologyAtCommit implements TriggerIndex. A path present as a directory or a
// blob that cannot be read is an error, never "try the next path", so a
// shadowing directory cannot fall through (the same rule as verify_fold's
// settingsAt).
func (rh *repoHandler) OntologyAtCommit(ctx context.Context, commit plumbing.Hash) (string, string, []byte, error) {
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return "", "", nil, fmt.Errorf("triggers: ontology at %s: %w", commit, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return "", "", nil, fmt.Errorf("triggers: tree of %s: %w", commit, err)
	}
	for _, p := range fact.OntologyPathsNewestFirst() {
		entry, err := tree.FindEntry(p)
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			continue
		}
		if err != nil {
			return "", "", nil, fmt.Errorf("triggers: ontology entry %q at %s: %w", p, commit, err)
		}
		if !entry.Mode.IsFile() {
			return "", "", nil, fmt.Errorf("triggers: ontology path %q at %s is not a file", p, commit)
		}
		f, err := tree.TreeEntryFile(entry)
		if err != nil {
			return "", "", nil, fmt.Errorf("triggers: ontology blob %q at %s: %w", p, commit, err)
		}
		body, err := f.Contents()
		if err != nil {
			return "", "", nil, fmt.Errorf("triggers: ontology contents %q at %s: %w", p, commit, err)
		}
		return p, entry.Hash.String(), []byte(body), nil
	}
	return "", "", nil, ErrNoOntologyAtCommit
}

// blobHashAt returns the blob hash path carries in c's tree, or ZeroHash when
// the path is absent there.
func blobHashAt(c *object.Commit, path string) (plumbing.Hash, error) {
	tree, err := c.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("triggers: tree of %s: %w", c.Hash, err)
	}
	entry, err := tree.FindEntry(path)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("triggers: entry %q at %s: %w", path, c.Hash, err)
	}
	return entry.Hash, nil
}

// Toucher implements TriggerIndex: the commit that introduced the blob `path`
// carries at head (or, for a path absent at head, the commit that removed it).
//
// The rule is git's own history simplification, restricted to one path and
// with no clock:
//   - start at head; at each commit compare the path's blob with its parents';
//   - follow the FIRST parent, in parent order, that carries the SAME blob
//     (the change came through that side, so merges are skipped);
//   - a commit with NO same-blob parent is the toucher: a non-merge whose
//     parent differs (the usual case), or a merge that produced the blob
//     itself (MergeToucher);
//   - a commit with no parent and no toucher found means the path never had
//     an origin in this history (a scrubbed path after a rewind): Found=false.
//
// It is NOT bounded with IsAncestor(candidate, W): that cost ~30 ms per path
// in the reviewer's probe. In a linear advance every learn/update/retract
// ends at a real toucher inside the advance; the dispatcher does not call it
// for a retract in a NONLINEAR advance, where the same-absent-blob walk would
// run to the root.
func (rh *repoHandler) Toucher(ctx context.Context, head plumbing.Hash, path string) (TouchResult, error) {
	cur, err := rh.repo.CommitObject(head)
	if err != nil {
		return TouchResult{}, fmt.Errorf("triggers: toucher head %s: %w", head, err)
	}
	curBlob, err := blobHashAt(cur, path)
	if err != nil {
		return TouchResult{}, err
	}
	res := TouchResult{}
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n := cur.NumParents()
		if n == 0 {
			return res, nil // Found stays false
		}
		var same *object.Commit
		for i := 0; i < n; i++ {
			p, err := cur.Parent(i)
			if err != nil {
				return res, fmt.Errorf("triggers: toucher parent %d of %s: %w", i, cur.Hash, err)
			}
			pb, err := blobHashAt(p, path)
			if err != nil {
				return res, err
			}
			if pb == curBlob {
				same = p
				break
			}
		}
		if same == nil {
			res.Commit, res.Found, res.MergeToucher = cur.Hash, true, n > 1
			return res, nil
		}
		res.Steps++
		cur = same
	}
}

// CommitInfo implements TriggerIndex.
func (rh *repoHandler) CommitInfo(ctx context.Context, hash plumbing.Hash) (CommitInfo, error) {
	c, err := rh.repo.CommitObject(hash)
	if err != nil {
		return CommitInfo{}, fmt.Errorf("triggers: commit %s: %w", hash, err)
	}
	return CommitInfo{
		Hash:        c.Hash,
		AuthorName:  c.Author.Name,
		AuthorEmail: c.Author.Email,
		Message:     c.Message,
		Parents:     append([]plumbing.Hash(nil), c.ParentHashes...),
	}, nil
}

// CommitSignerOf implements TriggerIndex.
func (rh *repoHandler) CommitSignerOf(ctx context.Context, hash plumbing.Hash) (CommitSigner, error) {
	c, err := rh.repo.CommitObject(hash)
	if err != nil {
		return CommitSigner{}, fmt.Errorf("triggers: commit %s: %w", hash, err)
	}
	return verifyCommitSignature(c)
}

// BlobAt implements TriggerIndex.
func (rh *repoHandler) BlobAt(ctx context.Context, commit plumbing.Hash, path string) (string, bool, error) {
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return "", false, fmt.Errorf("triggers: commit %s: %w", commit, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return "", false, fmt.Errorf("triggers: tree of %s: %w", commit, err)
	}
	f, err := tree.File(path)
	if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("triggers: file %q at %s: %w", path, commit, err)
	}
	content, err := f.Contents()
	if err != nil {
		return "", false, fmt.Errorf("triggers: contents %q at %s: %w", path, commit, err)
	}
	return content, true, nil
}

// IsAncestor implements TriggerIndex: a is b or an ancestor of b.
func (rh *repoHandler) IsAncestor(ctx context.Context, a, b plumbing.Hash) (bool, error) {
	return isAncestorCommit(rh, a, b)
}

// VerifiedAnchor implements TriggerIndex.
func (rh *repoHandler) VerifiedAnchor(ctx context.Context, upstream string) (plumbing.Hash, map[plumbing.Hash]bool, error) {
	ref, err := rh.gits.Reference(verifiedRefName(upstream))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil, nil
	}
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("triggers: read anchor: %w", err)
	}
	return ref.Hash(), rh.cachedBelow(upstream, ref.Hash()), nil
}

// AncestorSet implements TriggerIndex.
func (rh *repoHandler) AncestorSet(ctx context.Context, tip plumbing.Hash) (map[plumbing.Hash]bool, error) {
	iter, err := rh.repo.Log(&gogit.LogOptions{From: tip})
	if err != nil {
		return nil, fmt.Errorf("triggers: log from %s: %w", tip, err)
	}
	set := map[plumbing.Hash]bool{}
	err = iter.ForEach(func(c *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		set[c.Hash] = true
		return nil
	})
	if err != nil && !errors.Is(err, storer.ErrStop) {
		return nil, fmt.Errorf("triggers: walk from %s: %w", tip, err)
	}
	return set, nil
}

// TriggerWatermarks implements TriggerIndex.
func (rh *repoHandler) TriggerWatermarks(ctx context.Context, branch string) (map[string]string, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx,
		`SELECT trigger, commit_hash FROM trigger_watermarks WHERE branch = ?`, branch)
	if err != nil {
		return nil, fmt.Errorf("TriggerWatermarks: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, fmt.Errorf("TriggerWatermarks: %w", err)
		}
		out[name] = hash
	}
	return out, rows.Err()
}

// RecordTriggerRun implements TriggerIndex (tx1). One transaction, so the
// process-wide write lock (_txlock=immediate) is taken once, for at most
// TriggerFireRetention+1 short inserts, never across `if` or emit.
func (rh *repoHandler) RecordTriggerRun(ctx context.Context, run TriggerRun) (int, error) {
	rows := run.Rows
	notLogged := 0
	if len(rows) > TriggerFireRetention {
		notLogged = len(rows) - TriggerFireRetention
		rows = rows[notLogged:] // newest paths last: keep the last N
	}
	tx, err := rh.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("RecordTriggerRun: begin: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO trigger_fires
		(trigger, branch, path, episode, source, commit_hash, trace, outcome, error, nonlinear,
		 range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("RecordTriggerRun: prepare: %w", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.Trigger, run.Branch, r.Path, r.Episode, r.Source, r.Commit, r.Trace,
			r.Outcome, r.Error, boolInt(r.Nonlinear), run.RangeFrom, run.RangeTo, 0, 0, 0, 0, 0, 0, 0, now); err != nil {
			return 0, fmt.Errorf("RecordTriggerRun: fire row: %w", err)
		}
	}
	if _, err := stmt.ExecContext(ctx, "", run.Branch, "", "", "", "", "", TriggerOutcomeRun, "", boolInt(run.Nonlinear),
		run.RangeFrom, run.RangeTo, run.Evaluated, run.Paths, run.Fires, notLogged,
		run.DurationMS, run.DiffMS, run.ChangeMS, now); err != nil {
		return 0, fmt.Errorf("RecordTriggerRun: run row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("RecordTriggerRun: commit: %w", err)
	}
	return len(rows), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AdvanceTriggerWatermarks implements TriggerIndex (tx2).
func (rh *repoHandler) AdvanceTriggerWatermarks(ctx context.Context, branch string, set map[string]string, del []string) error {
	tx, err := rh.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AdvanceTriggerWatermarks: begin: %w", err)
	}
	defer tx.Rollback()
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO trigger_watermarks(trigger, branch, commit_hash) VALUES (?, ?, ?)`,
			n, branch, set[n]); err != nil {
			return fmt.Errorf("AdvanceTriggerWatermarks: set %q: %w", n, err)
		}
	}
	for _, n := range del {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM trigger_watermarks WHERE trigger = ? AND branch = ?`, n, branch); err != nil {
			return fmt.Errorf("AdvanceTriggerWatermarks: delete %q: %w", n, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM trigger_fires WHERE id <= (SELECT COALESCE(MAX(id), 0) FROM trigger_fires) - ?`,
		TriggerFireRetention); err != nil {
		return fmt.Errorf("AdvanceTriggerWatermarks: prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("AdvanceTriggerWatermarks: commit: %w", err)
	}
	return nil
}

// RecentTriggerFires implements TriggerIndex.
func (rh *repoHandler) RecentTriggerFires(ctx context.Context, branch string, limit int) ([]TriggerFire, error) {
	if limit <= 0 {
		return []TriggerFire{}, nil
	}
	rows, err := conn(ctx, rh.db).QueryContext(ctx, `SELECT id, trigger, branch, path, episode, source, commit_hash, trace,
		outcome, error, nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged,
		duration_ms, diff_ms, change_ms, fired_at
		FROM trigger_fires WHERE branch = ? ORDER BY id DESC LIMIT ?`, branch, limit)
	if err != nil {
		return nil, fmt.Errorf("RecentTriggerFires: %w", err)
	}
	defer rows.Close()
	out := []TriggerFire{}
	for rows.Next() {
		var f TriggerFire
		var nonlinear int
		if err := rows.Scan(&f.ID, &f.Trigger, &f.Branch, &f.Path, &f.Episode, &f.Source, &f.Commit, &f.Trace,
			&f.Outcome, &f.Error, &nonlinear, &f.RangeFrom, &f.RangeTo, &f.Evaluated, &f.Paths, &f.Fires,
			&f.FiresNotLogged, &f.DurationMS, &f.DiffMS, &f.ChangeMS, &f.FiredAt); err != nil {
			return nil, fmt.Errorf("RecentTriggerFires: %w", err)
		}
		f.Nonlinear = nonlinear != 0
		out = append(out, f)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("RecentTriggerFires: %w", err)
	}
	return out, nil
}
