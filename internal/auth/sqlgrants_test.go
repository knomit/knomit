package auth

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openGrantsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// The same DDL as 000009_grants.up.sql; that the MIGRATION carries it is
	// asserted separately by internal/store/migrate/control_test.go.
	if _, err := db.Exec(`CREATE TABLE grants (
		principal TEXT NOT NULL, permission TEXT NOT NULL, granted_by TEXT NOT NULL DEFAULT '',
		granted_at INTEGER NOT NULL, revoked_at INTEGER, PRIMARY KEY (principal, permission))`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLGrants_GrantRevokeRoundTrip(t *testing.T) {
	ctx := context.Background()
	g := NewSQLGrants(openGrantsDB(t))
	p := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaSocket}

	set, err := g.For(ctx, p)
	if err != nil || len(set) != 0 {
		t.Fatalf("fresh principal must have nothing: %v %v", set, err)
	}
	if err := g.Grant(ctx, p, Read, "test"); err != nil {
		t.Fatal(err)
	}
	if err := g.Grant(ctx, p, Read, "test"); err != nil {
		t.Fatalf("re-grant must be idempotent: %v", err)
	}
	set, _ = g.For(ctx, p)
	if !set.Has(Read) || set.Has(Write) {
		t.Fatalf("after grant: %v", set)
	}
	if err := g.Revoke(ctx, p, Read); err != nil {
		t.Fatal(err)
	}
	set, _ = g.For(ctx, p)
	if set.Has(Read) {
		t.Fatal("revoked permission must not be returned")
	}
	if err := g.Grant(ctx, p, Read, "test"); err != nil {
		t.Fatalf("re-grant after revoke must clear revoked_at: %v", err)
	}
	set, _ = g.For(ctx, p)
	if !set.Has(Read) {
		t.Fatal("re-granted permission must be live again")
	}
}

// A revoked row must stay readable as history: boot distinguishes "never
// granted" from "revoked" so a restart never revives what an operator took.
func TestSQLGrants_EverGrantedSurvivesRevocation(t *testing.T) {
	ctx := context.Background()
	g := NewSQLGrants(openGrantsDB(t))
	p := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaSocket}

	ever, err := g.EverGranted(ctx, p, Write)
	if err != nil || ever {
		t.Fatalf("never granted must be false: %v %v", ever, err)
	}
	if err := g.Grant(ctx, p, Write, "boot:loopback_default"); err != nil {
		t.Fatal(err)
	}
	if err := g.Revoke(ctx, p, Write); err != nil {
		t.Fatal(err)
	}
	set, _ := g.For(ctx, p)
	if set.Has(Write) {
		t.Fatal("revoked write must not be live")
	}
	if ever, err = g.EverGranted(ctx, p, Write); err != nil || !ever {
		t.Fatalf("a revoked row must still count as ever-granted: %v %v", ever, err)
	}
}

// The via is part of the key: the same uid over a token is a different
// principal from the same uid over the socket, and must not inherit grants.
func TestSQLGrants_ViaIsPartOfTheKey(t *testing.T) {
	ctx := context.Background()
	g := NewSQLGrants(openGrantsDB(t))
	viaSocket := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaSocket}
	viaToken := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaToken}
	if err := g.Grant(ctx, viaSocket, Write, "test"); err != nil {
		t.Fatal(err)
	}
	set, _ := g.For(ctx, viaToken)
	if set.Has(Write) {
		t.Fatal("a grant to the socket principal must not reach the token principal")
	}
}
