//go:build !sqlite_vtable

// Stubs for builds without the sqlite_vtable tag. The vtab is unavailable;
// any code path that requires the resolver to run will need the tag.
package store

import sqlite3 "github.com/mattn/go-sqlite3"

func registerVTabs(_ *sqlite3.SQLiteConn) error { return nil }
func bindVTabRepo(_ string, _ *repoHandler)     {}
func unbindVTabRepo(_ string)                   {}
func lookupVTabRepo(_ string) *repoHandler      { return nil }
