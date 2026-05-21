//go:build sqlite_vtable

// Custom SQLite virtual table that streams first-parent ancestry from a
// given commit, backed by the per-Service repoHandler.
//
// Registered per-connection in the sqlite3_knomit ConnectHook. The handler
// is looked up lazily at query time via a path-keyed registry so that the
// ConnectHook does not need to know about repoHandler at driver-registration
// time (which happens before any Service exists).
package store

import "sync"

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
