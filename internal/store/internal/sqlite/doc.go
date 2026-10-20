// Package sqlite is the bottom of the store's internal layering: everything
// that is about the SQLite engine itself and knows nothing about repoHandler
// or any sub-index. It owns the "sqlite3_knomit" driver (sqlite-vec plus
// knomit's SQL functions and per-connection pragmas), the float32 vector codec
// and cosine definition shared by Go and SQL, the ctx-carried transaction
// helpers (Conn, BeginTxIfNeeded), the test-only durability switch, credential
// encryption (Crypt) and the donated-embedding context key.
//
// It must not import its parent package store: the parent re-exports what
// callers outside store need (Crypt, CosineSim, SyncDSNParam, ...) and
// forwards the rest under the old unexported names.
package sqlite
