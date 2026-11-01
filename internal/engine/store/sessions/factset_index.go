package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"

	"knomit/internal/engine/store/model"
)

// FactSet cursors: the session-DB half of a materialized search
// (internal/corpus/materialize). A FactSet is a tool_sessions row (owner,
// binding pin, read set, the idle reaper) plus ONE factsets row holding its
// frozen inputs. Both are written ONCE, at creation, in one transaction; the
// reading position travels in the cursor token and the walk's state is an
// in-memory cache, so paging never writes here. The only later write is the
// bounded heartbeat (TouchFactSet), at most once per quarter of the idle TTL.
//
// Every write goes through factsetWrite: BEGIN IMMEDIATE on a dedicated
// connection, the DequeuePaths pattern (invariant a8e8e8e4: a read-then-write
// on the session DB must take the write lock up front, or its first write
// after a concurrent commit fails "database is locked" at once). A lock that
// could not be taken within the busy timeout is model.ErrSessionBusy: nothing
// was written and the caller may retry.

// FactSetToolPrefix prefixes the tool_sessions.tool value of a FactSet
// cursor: "materialize-factset:<limit>".
const FactSetToolPrefix = "materialize-factset:"

// FactSetRow is a FactSet's session row as every call reads it: the scope a
// resume must match, the frozen inputs, and the heartbeat.
type FactSetRow struct {
	ID       string
	Tool     string
	Branch   string
	Binding  string
	ReadSet  string
	Owner    string
	Status   string
	Frozen   []byte
	LastUsed time.Time
}

// factsetWrites counts the transactions factsetWrite committed, per index.
type factsetWrites struct{ n atomic.Int64 }

// FactSetWrites is how many writes the FactSet code made to this session DB
// (creations, heartbeats, deletions): the counter a test pins at "one per
// FactSet plus heartbeats".
func (ti *ToolIndex) FactSetWrites() int64 { return ti.fsWrites.n.Load() }

func isBusy(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && (se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked)
}

// factsetWrite runs fn in BEGIN IMMEDIATE on a connection of its own and
// commits; any failure rolls back, under a context that cannot be cancelled
// so a cancelled request never leaves the lock held.
func (ti *ToolIndex) factsetWrite(ctx context.Context, op string, fn func(c *sql.Conn) error) error {
	c, err := ti.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s conn: %w", op, err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s: %w: %w", op, model.ErrSessionBusy, err)
		}
		return fmt.Errorf("%s begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if err := fn(c); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s: %w: %w", op, model.ErrSessionBusy, err)
		}
		return err
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s commit: %w: %w", op, model.ErrSessionBusy, err)
		}
		return fmt.Errorf("%s commit: %w", op, err)
	}
	committed = true
	ti.fsWrites.n.Add(1)
	return nil
}

// CreateFactSet writes a new FactSet cursor in ONE transaction: the session
// row and the frozen inputs. It only inserts: an owner holds any number of
// FactSets at once, and a FactSet ends only by CloseFactSet, the idle reaper,
// or the session DB wipe on store open.
func (ti *ToolIndex) CreateFactSet(ctx context.Context, tool, branch, binding, readSet, owner string, frozen []byte) (string, error) {
	if !strings.HasPrefix(tool, FactSetToolPrefix) {
		return "", fmt.Errorf("CreateFactSet: tool %q is not a factset tool", tool)
	}
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	err := ti.factsetWrite(ctx, "CreateFactSet", func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO tool_sessions(id, tool, branch, path_prefix, binding, read_set, owner, last_commit, status, created_at, updated_at, last_used_at)
			 VALUES (?, ?, ?, '', ?, ?, ?, '', 'active', ?, ?, ?)`,
			id, tool, branch, binding, readSet, owner, now, now, now); err != nil {
			return fmt.Errorf("CreateFactSet session: %w", err)
		}
		if _, err := c.ExecContext(ctx, `INSERT INTO factsets(session_id, frozen) VALUES (?, ?)`, id, frozen); err != nil {
			return fmt.Errorf("CreateFactSet frozen: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetFactSet reads a FactSet cursor's row; nil when there is none (unknown,
// reaped, closed, or another tool's session). A read only: no write.
func (ti *ToolIndex) GetFactSet(ctx context.Context, id string) (*FactSetRow, error) {
	var r FactSetRow
	var lastUsed string
	err := ti.db.QueryRowContext(ctx,
		`SELECT s.id, s.tool, s.branch, s.binding, s.read_set, s.owner, s.status, s.last_used_at, f.frozen
		   FROM tool_sessions s JOIN factsets f ON f.session_id = s.id
		  WHERE s.id = ? AND s.tool LIKE ?`, id, FactSetToolPrefix+"%").
		Scan(&r.ID, &r.Tool, &r.Branch, &r.Binding, &r.ReadSet, &r.Owner, &r.Status, &lastUsed, &r.Frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetFactSet: %w", err)
	}
	if r.LastUsed, err = time.Parse(time.RFC3339, lastUsed); err != nil {
		return nil, fmt.Errorf("GetFactSet: last_used_at: %w", err)
	}
	return &r, nil
}

// TouchFactSet writes the heartbeat (last_used_at = now) so the idle reaper
// keeps an actively read FactSet. The caller bounds how often it is called.
func (ti *ToolIndex) TouchFactSet(ctx context.Context, id string, now time.Time) error {
	return ti.factsetWrite(ctx, "TouchFactSet", func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, now.UTC().Format(time.RFC3339), id)
		return err
	})
}

// DeleteFactSet removes a FactSet cursor (the cascade drops its frozen row).
func (ti *ToolIndex) DeleteFactSet(ctx context.Context, id string) error {
	return ti.factsetWrite(ctx, "DeleteFactSet", func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM tool_sessions WHERE id = ?`, id)
		return err
	})
}

// CloseFactSet deletes the FactSet id ONLY when it is an active FactSet of
// exactly this binding pin, read set and owner, in one statement (the cascade
// drops its frozen row). It reports whether a row was deleted: false for an
// unknown id and for another owner's, pin's or read set's FactSet alike, so
// the caller answers both the same way.
func (ti *ToolIndex) CloseFactSet(ctx context.Context, id, binding, readSet, owner string) (bool, error) {
	if binding == "" || owner == "" {
		return false, nil
	}
	var closed bool
	err := ti.factsetWrite(ctx, "CloseFactSet", func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx,
			`DELETE FROM tool_sessions
			  WHERE id = ? AND tool LIKE ? AND status = 'active' AND binding = ? AND read_set = ? AND owner = ?`,
			id, FactSetToolPrefix+"%", binding, readSet, owner)
		if err != nil {
			return fmt.Errorf("CloseFactSet: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("CloseFactSet: %w", err)
		}
		closed = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return closed, nil
}

// closeHooks run once when the store owning this index closes.
type closeHooks struct {
	mu    sync.Mutex
	fns   []func()
	ran   bool
	added map[string]bool
}

// OnClose registers fn to run once when this index's store closes, under key
// (a second registration under the same key is ignored). A cache of state
// derived from this store drops it there.
func (ti *ToolIndex) OnClose(key string, fn func()) {
	ti.hooks.mu.Lock()
	defer ti.hooks.mu.Unlock()
	if ti.hooks.ran {
		return
	}
	if ti.hooks.added == nil {
		ti.hooks.added = map[string]bool{}
	}
	if ti.hooks.added[key] {
		return
	}
	ti.hooks.added[key] = true
	ti.hooks.fns = append(ti.hooks.fns, fn)
}

// RunCloseHooks runs the registered close hooks once. Service.Close calls it.
func (ti *ToolIndex) RunCloseHooks() {
	ti.hooks.mu.Lock()
	fns := ti.hooks.fns
	ti.hooks.fns, ti.hooks.ran = nil, true
	ti.hooks.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}
