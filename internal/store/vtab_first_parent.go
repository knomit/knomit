//go:build sqlite_vtable

// Custom SQLite virtual table that streams first-parent ancestry from a
// given commit, backed by the per-Service repoHandler.
//
// Registered per-connection in the sqlite3_knomit ConnectHook. The handler
// is looked up lazily at query time via a path-keyed registry so that the
// ConnectHook does not need to know about repoHandler at driver-registration
// time (which happens before any Service exists).
package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

var (
	vtabRegistryMu sync.RWMutex
	vtabRegistry   = map[string]*repoHandler{}
)

// bindVTabRepo associates a database file path with a repoHandler so that
// any vtab cursor opened on that connection can find the git repo it needs.
// Called from store.Open after the repoHandler is constructed.
func bindVTabRepo(path string, rh *repoHandler) {
	vtabRegistryMu.Lock()
	defer vtabRegistryMu.Unlock()
	vtabRegistry[path] = rh
}

// unbindVTabRepo removes the binding. Called from (*Service).Close.
func unbindVTabRepo(path string) {
	vtabRegistryMu.Lock()
	defer vtabRegistryMu.Unlock()
	delete(vtabRegistry, path)
}

// lookupVTabRepo returns the bound repoHandler for path, or nil if no
// binding exists (e.g. the vtab was queried before Open finished, or after
// Close ran).
func lookupVTabRepo(path string) *repoHandler {
	vtabRegistryMu.RLock()
	defer vtabRegistryMu.RUnlock()
	return vtabRegistry[path]
}

// firstParentChainModule is an eponymous-only module: used by name in queries
// as a table-valued function, not instantiable via CREATE VIRTUAL TABLE.
// Each connection gets its own module instance with the db path baked in so
// the cursor can find its repoHandler at query time via the registry.
type firstParentChainModule struct {
	dbPath string
}

func newFirstParentChainModule(dbPath string) *firstParentChainModule {
	return &firstParentChainModule{dbPath: dbPath}
}

// EponymousOnlyModule is the marker method required by mattn/go-sqlite3
// to flag this as a table-valued function (no CREATE VIRTUAL TABLE).
func (m *firstParentChainModule) EponymousOnlyModule() {}

func (m *firstParentChainModule) Create(c *sqlite3.SQLiteConn, args []string) (sqlite3.VTab, error) {
	// Hidden column from_commit lets the caller bind the start hash via
	// table-valued-function syntax: SELECT * FROM first_parent_chain('hash').
	if err := c.DeclareVTab(
		`CREATE TABLE x(commit_hash TEXT, depth INTEGER, from_commit HIDDEN)`,
	); err != nil {
		return nil, fmt.Errorf("first_parent_chain: declare: %w", err)
	}
	return &firstParentChainTable{dbPath: m.dbPath}, nil
}

func (m *firstParentChainModule) Connect(c *sqlite3.SQLiteConn, args []string) (sqlite3.VTab, error) {
	return m.Create(c, args)
}

func (m *firstParentChainModule) DestroyModule() {}

type firstParentChainTable struct {
	dbPath string
}

func (t *firstParentChainTable) BestIndex(cs []sqlite3.InfoConstraint, ob []sqlite3.InfoOrderBy) (*sqlite3.IndexResult, error) {
	used := make([]bool, len(cs))
	fromIdx := -1
	for i, c := range cs {
		// Column 2 = from_commit (hidden). OpEQ = equality.
		if c.Column == 2 && c.Op == sqlite3.OpEQ && c.Usable {
			used[i] = true
			fromIdx = i
		}
	}
	if fromIdx < 0 {
		return nil, fmt.Errorf("first_parent_chain: from_commit constraint required")
	}
	return &sqlite3.IndexResult{
		IdxNum:         1,
		Used:           used,
		AlreadyOrdered: true, // we naturally emit by depth ASC
		EstimatedCost:  1.0,
		EstimatedRows:  16,
	}, nil
}

func (t *firstParentChainTable) Open() (sqlite3.VTabCursor, error) {
	return &firstParentChainCursor{dbPath: t.dbPath}, nil
}

func (t *firstParentChainTable) Disconnect() error { return nil }
func (t *firstParentChainTable) Destroy() error    { return nil }

type firstParentChainCursor struct {
	dbPath string
	rh     *repoHandler
	cur    string
	depth  int64
	eof    bool
}

func (c *firstParentChainCursor) Filter(idxNum int, idxStr string, vals []any) error {
	if idxNum != 1 || len(vals) != 1 {
		return fmt.Errorf("first_parent_chain: bad index plan idxNum=%d argc=%d", idxNum, len(vals))
	}
	rh := lookupVTabRepo(c.dbPath)
	if rh == nil {
		return fmt.Errorf("first_parent_chain: no repoHandler bound for db %s", c.dbPath)
	}
	c.rh = rh

	from, _ := vals[0].(string)
	c.cur = from
	c.depth = 0
	c.eof = (from == "")
	return nil
}

func (c *firstParentChainCursor) Next() error {
	if c.eof {
		return nil
	}
	parent, err := c.rh.firstParentCommit(context.TODO(), c.cur)
	if err != nil {
		return fmt.Errorf("first_parent_chain: firstParentCommit: %w", err)
	}
	if parent == "" {
		c.eof = true
		return nil
	}
	c.cur = parent
	c.depth++
	return nil
}

func (c *firstParentChainCursor) EOF() bool { return c.eof }

func (c *firstParentChainCursor) Column(ctx *sqlite3.SQLiteContext, col int) error {
	switch col {
	case 0:
		ctx.ResultText(c.cur)
	case 1:
		ctx.ResultInt64(c.depth)
	case 2:
		// Hidden from_commit — echo back the current hash.
		ctx.ResultText(c.cur)
	default:
		return fmt.Errorf("first_parent_chain: unknown column %d", col)
	}
	return nil
}

func (c *firstParentChainCursor) Rowid() (int64, error) { return c.depth, nil }
func (c *firstParentChainCursor) Close() error          { return nil }

// registerVTabs is called from the ConnectHook for every new connection.
// It resolves the connection's database file path and registers the
// first_parent_chain module bound to that path.
func registerVTabs(conn *sqlite3.SQLiteConn) error {
	path, err := mainDatabasePath(conn)
	if err != nil {
		return fmt.Errorf("registerVTabs: db path: %w", err)
	}
	return conn.CreateModule("first_parent_chain", newFirstParentChainModule(path))
}

// mainDatabasePath returns the on-disk path of the connection's main db
// via PRAGMA database_list. Returns "" for in-memory dbs.
func mainDatabasePath(conn *sqlite3.SQLiteConn) (string, error) {
	rows, err := conn.Query("PRAGMA database_list", nil)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	// Columns: seq, name, file.
	for {
		vals := make([]driver.Value, 3)
		if err := rows.Next(vals); err != nil {
			// EOF or error — in-memory db returns empty file column.
			return "", nil
		}
		if name, _ := vals[1].(string); name == "main" {
			if p, _ := vals[2].(string); p != "" {
				return p, nil
			}
			return "", nil
		}
	}
}
