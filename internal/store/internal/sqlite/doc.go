// Package sqlite is the bottom of the store's internal layering: everything
// that is about the SQLite engine itself and knows nothing about repoHandler
// or any sub-index. It owns the "sqlite3_knomit" driver (sqlite-vec plus
// knomit's SQL functions and per-connection pragmas), the float32 vector codec
// and the ctx-carried transaction helpers (Conn, BeginTxIfNeeded), the
// test-only durability switch and its DSN parameter. Credential encryption
// (Crypt), the cosine definition and the donated-embedding context key live in
// store/model, which this package imports.
//
// It must not import its parent package store: the parent keeps SyncDSNParam
// (package state callers outside store read) and forwards the rest under the
// old unexported names.
package sqlite
