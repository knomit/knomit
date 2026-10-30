package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"

	"knomit/internal/engine/store/model"
)

// FactSet cursors: the session-DB half of "materialize a search as a factset". A
// factset is a tool_sessions row (so ownership, binding pin, read set and the
// idle reaper apply unchanged) plus a factset_sessions row (its envelope and
// reading position). Its structure is held in memory, and spilled to a FILE
// of its own only when it must (factset_store.go, factset_file.go), so a factset never
// contends with another factset or another cursor on the shared session DB.
//
// WRITES to the shared session DB are small (one session row, one position
// update per page) and each is one BEGIN IMMEDIATE transaction on a
// dedicated connection (factsetWrite), the createPipelineSession / DequeuePaths
// pattern: a read-then-write never upgrades a deferred lock (invariant
// a8e8e8e4). factsetWrite is the ONE seam every factset write to the shared DB goes
// through, so an in-process writer gate can be added there without touching a
// caller; it records each write's lock wait (FactSetLockWaits). A lock wait that
// times out is ErrBusy, which an edge answers 503 with Retry-After.

// FactSetToolPrefix prefixes the tool_sessions.tool value of a factset cursor:
// "materialize-factset:<limit>".
const FactSetToolPrefix = "materialize-factset:"

// ErrBusy is a session-DB write that could not take the write lock within
// the busy timeout. Nothing was written; the caller may retry.
var ErrBusy = model.ErrSessionBusy

// FactSetInfo is a factset's envelope and cursor position.
type FactSetInfo struct {
	Env     string
	Units   int
	NextSeq int
}

// factsetLockWaits is the lock-wait record of every factset write on this index.
type factsetLockWaits struct {
	count, totalNs, maxNs atomic.Int64
}

func (w *factsetLockWaits) record(d time.Duration) {
	w.count.Add(1)
	w.totalNs.Add(int64(d))
	for {
		cur := w.maxNs.Load()
		if int64(d) <= cur || w.maxNs.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}

// FactSetLockWaits reports how many factset writes to the shared session DB ran
// on this index, their total and their longest wait for its write lock.
func (ti *ToolIndex) FactSetLockWaits() (count int64, total, max time.Duration) {
	return ti.waits.count.Load(), time.Duration(ti.waits.totalNs.Load()), time.Duration(ti.waits.maxNs.Load())
}

// isBusy reports whether err is SQLite's "database is locked".
func isBusy(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked
	}
	return false
}

// factsetWrite runs fn in BEGIN IMMEDIATE on a connection of its own, and
// commits; any failure rolls back (under a context that cannot be cancelled,
// so a cancelled request never leaves the lock held). A lock that could not
// be taken is ErrBusy.
func (ti *ToolIndex) factsetWrite(ctx context.Context, op string, fn func(c *sql.Conn) error) error {
	c, err := ti.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s conn: %w", op, err)
	}
	defer c.Close()
	start := time.Now()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s: %w", op, ErrBusy)
		}
		return fmt.Errorf("%s begin: %w", op, err)
	}
	ti.waits.record(time.Since(start))
	committed := false
	defer func() {
		if !committed {
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if err := fn(c); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s: %w", op, ErrBusy)
		}
		return err
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s commit: %w", op, ErrBusy)
		}
		return fmt.Errorf("%s commit: %w", op, err)
	}
	committed = true
	return nil
}

// CreateFactSet starts a factset cursor for owner. It deletes owner's other factset
// cursors and frees their factsets (one active factset per endpoint + principal: a
// new first call replaces the old one), creates the session and its
// envelope, and registers the new factset IN MEMORY with its results (the
// frozen search results, unit 1..len, with the anchors reserved for their
// roots). No file is created until the factset must spill. On any failure
// nothing is left behind.
func (ti *ToolIndex) CreateFactSet(ctx context.Context, tool, branch, binding, readSet, owner, env string, results []FactSetResult, reserved []FactSetReserve) (string, error) {
	if !strings.HasPrefix(tool, FactSetToolPrefix) {
		return "", fmt.Errorf("CreateFactSet: tool %q is not a factset tool", tool)
	}
	if ti.factsetsDir == "" {
		return "", errors.New("CreateFactSet: this session store has no factset directory")
	}
	id := uuid.New().String()
	var replaced []string
	now := time.Now().UTC().Format(time.RFC3339)
	err := ti.factsetWrite(ctx, "CreateFactSet", func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx, `SELECT id FROM tool_sessions WHERE owner = ? AND tool LIKE ?`, owner, FactSetToolPrefix+"%")
		if err != nil {
			return fmt.Errorf("CreateFactSet replace: %w", err)
		}
		for rows.Next() {
			var old string
			if err := rows.Scan(&old); err != nil {
				rows.Close()
				return err
			}
			replaced = append(replaced, old)
		}
		rows.Close()
		if _, err := c.ExecContext(ctx,
			`DELETE FROM tool_sessions WHERE owner = ? AND tool LIKE ?`, owner, FactSetToolPrefix+"%"); err != nil {
			return fmt.Errorf("CreateFactSet replace: %w", err)
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO tool_sessions(id, tool, branch, path_prefix, binding, read_set, owner, last_commit, status, created_at, updated_at, last_used_at)
			 VALUES (?, ?, ?, '', ?, ?, ?, '', 'active', ?, ?, ?)`,
			id, tool, branch, binding, readSet, owner, now, now, now); err != nil {
			return fmt.Errorf("CreateFactSet session: %w", err)
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO factset_sessions(session_id, env, units) VALUES (?, ?, ?)`, id, env, len(results)); err != nil {
			return fmt.Errorf("CreateFactSet factset: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	bs := newMemFactSet(id, ti.factsetPath(id), results, reserved)
	ti.factsetsMu.Lock()
	ti.factsets[id] = bs
	var old []*factsetStore
	var lost []string
	for _, o := range replaced {
		b, known := ti.retireLocked(o)
		if b != nil {
			old = append(old, b)
		}
		if !known {
			lost = append(lost, o)
		}
	}
	ti.factsetsMu.Unlock()
	for _, b := range old {
		b.drop()
	}
	for _, o := range lost {
		removeFactSetFile(ti.factsetPath(o))
	}
	enforceFactSetBudgetLogged(ctx)
	return id, nil
}

// DeleteFactSet removes a factset cursor and frees its factset (memory or file), or,
// while a call holds it, retires it for the last holder's release to free.
func (ti *ToolIndex) DeleteFactSet(ctx context.Context, id string) error {
	err := ti.factsetWrite(ctx, "DeleteFactSet", func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM tool_sessions WHERE id = ?`, id)
		return err
	})
	ti.forgetFactSet(id)
	return err
}

// forgetFactSet takes factset id out of the registry and frees it, or, while a
// call holds it, leaves that to the holder's release (retireLocked).
func (ti *ToolIndex) forgetFactSet(id string) {
	ti.factsetsMu.Lock()
	bs, known := ti.retireLocked(id)
	ti.factsetsMu.Unlock()
	if bs != nil {
		bs.drop()
	}
	if !known {
		removeFactSetFile(ti.factsetPath(id))
	}
}

// retireLocked takes factset id out of service; factsetsMu is held. Every drop
// path that can meet a held factset goes through it (replacement, DeleteFactSet,
// CloseFactSets; the sweeper skips held factsets), following the origin-session
// finalize rule (invariant ad44382e):
//   - a factset no call holds leaves the registry at once and is returned, for
//     the caller to drop outside the lock;
//   - a factset a call holds is only marked retired: OpenFactSet refuses it (its
//     cursor reads as expired), its holders keep reading it, and the LAST
//     holder's release drops it.
//
// A factset is therefore never freed under a call that holds it. known is false
// when the registry has no factset id (lost with a restart): only then may the
// caller delete a file at its path.
func (ti *ToolIndex) retireLocked(id string) (dropNow *factsetStore, known bool) {
	bs := ti.factsets[id]
	if bs == nil {
		return nil, false
	}
	if bs.holders > 0 {
		bs.retired = true
		return nil, true
	}
	delete(ti.factsets, id)
	return bs, true
}

// GetFactSet returns a factset's envelope and position, nil when id is not a factset.
func (ti *ToolIndex) GetFactSet(ctx context.Context, id string) (*FactSetInfo, error) {
	var b FactSetInfo
	err := ti.db.QueryRowContext(ctx,
		`SELECT env, units, next_seq FROM factset_sessions WHERE session_id = ?`, id,
	).Scan(&b.Env, &b.Units, &b.NextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetFactSet: %w", err)
	}
	return &b, nil
}

// AdvanceFactSet moves the cursor from seq from to seq to, only if it is still
// at from (a compare-and-swap: a cursor has one reader at a time, and a
// concurrent call on the same cursor loses instead of serving the same rows
// twice), and bumps the idle-reaper heartbeat. ok is false when the cursor
// moved meanwhile or is gone.
func (ti *ToolIndex) AdvanceFactSet(ctx context.Context, id string, from, to int) (bool, error) {
	ok := false
	err := ti.factsetWrite(ctx, "AdvanceFactSet", func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx,
			`UPDATE factset_sessions SET next_seq = ? WHERE session_id = ? AND next_seq = ?`, to, id, from)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n == 1
		if !ok {
			return nil
		}
		now := time.Now().UTC().Format(time.RFC3339)
		_, err = c.ExecContext(ctx, `UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, now, id)
		return err
	})
	return ok, err
}

// TouchFactSet bumps the heartbeat without moving the cursor (an export reads
// the whole factset and leaves the position where the reader left it).
func (ti *ToolIndex) TouchFactSet(ctx context.Context, id string) error {
	return ti.factsetWrite(ctx, "TouchFactSet", func(c *sql.Conn) error {
		now := time.Now().UTC().Format(time.RFC3339)
		_, err := c.ExecContext(ctx, `UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, now, id)
		return err
	})
}

// OpenFactSet holds factset id for this call. ok is false when the factset is gone
// (replaced, deleted, expired, evicted, or lost with a restart), a retired
// factset another call still holds included. While held, no path frees the
// factset: the hold is registered under factsetsMu, the lock every drop path
// takes (retireLocked, the sweeper); Close releases it, and the last
// release of a retired factset frees it.
func (ti *ToolIndex) OpenFactSet(id string) (*FactSet, bool) {
	ti.factsetsMu.Lock()
	defer ti.factsetsMu.Unlock()
	bs := ti.factsets[id]
	if bs == nil || bs.retired {
		return nil, false
	}
	bs.holders++
	return &FactSet{bs: bs, release: func() {
		ti.factsetsMu.Lock()
		bs.holders--
		last := bs.retired && bs.holders == 0
		if last && ti.factsets[bs.id] == bs {
			delete(ti.factsets, bs.id)
		}
		ti.factsetsMu.Unlock()
		if last {
			bs.drop()
		}
	}}, true
}

// FactSetFiles reports the factset files on disk and their total size in bytes.
func (ti *ToolIndex) FactSetFiles() (files int, bytes int64, err error) {
	if ti.factsetsDir == "" {
		return 0, 0, nil
	}
	entries, err := os.ReadDir(ti.factsetsDir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), factsetFileSuffix) {
			continue
		}
		files++
		if fi, err := e.Info(); err == nil {
			bytes += fi.Size()
		}
	}
	return files, bytes, nil
}

// FactSetCount reports how many factset cursors the session DB holds.
func (ti *ToolIndex) FactSetCount(ctx context.Context) (int, error) {
	var n int
	if err := ti.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM factset_sessions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("FactSetCount: %w", err)
	}
	return n, nil
}

// LiveFactSets reports the factsets this index holds: in memory and spilled.
func (ti *ToolIndex) LiveFactSets() (inMemory, spilled int) {
	ti.factsetsMu.Lock()
	defer ti.factsetsMu.Unlock()
	for _, bs := range ti.factsets {
		if bs.spilled() {
			spilled++
		} else {
			inMemory++
		}
	}
	return inMemory, spilled
}

func (ti *ToolIndex) factsetPath(id string) string {
	return filepath.Join(ti.factsetsDir, filepath.Base(id)+factsetFileSuffix)
}

// OpenFactSetFiles is how many SQLite handles spilled factsets hold right now
// (a spilled factset keeps its file open until it is dropped).
func (ti *ToolIndex) OpenFactSetFiles() int {
	_, spilled := ti.LiveFactSets()
	return spilled
}

// FactSetFilePath is the file of factset id (for tests and diagnostics).
func (ti *ToolIndex) FactSetFilePath(id string) string { return ti.factsetPath(id) }

// CloseFactSets frees every live factset of this index (the store is closing):
// their memory leaves the process budget and their files are closed. A factset
// a call still holds is retired instead, and freed on its last release.
func (ti *ToolIndex) CloseFactSets() {
	ti.factsetsMu.Lock()
	var drop []*factsetStore
	for id := range ti.factsets {
		if bs, _ := ti.retireLocked(id); bs != nil {
			drop = append(drop, bs)
		}
	}
	ti.factsetsMu.Unlock()
	for _, bs := range drop {
		bs.drop()
	}
}
