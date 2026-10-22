package cmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"knomit/internal/corpus/repos"

	"knomit/internal/corpus/repos/control"
)

// run executes the real root command with args and returns stdout+stderr.
func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := RootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// instanceHome makes a knomit home holding an instance key written the way
// ensureKeyPair writes it, with the .pub line `knomit serve` prints.
func instanceHome(t *testing.T, host string) (home string, pub ed25519.PublicKey, pubPath string) {
	t.Helper()
	home = t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	os.WriteFile(filepath.Join(home, "id_ed25519"), pem.EncodeToMemory(block), 0o600)
	sshPub, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " knomit@" + host + "\n"
	pubPath = filepath.Join(home, "id_ed25519.pub")
	os.WriteFile(pubPath, []byte(line), 0o644)
	return home, pub, pubPath
}

// useHome points config.Load at home and neutralises every env override that
// would otherwise reach a developer's REAL key or pki dir from their shell.
func useHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("KNOMIT_HOME", home)
	for _, k := range []string{"KNOMIT_REMOTE_SSH_KEY", "KNOMIT_TLS_ADDR", "KNOMIT_TLS_DIR"} {
		t.Setenv(k, "")
	}
}

// `identity` is a command group. Without its own Args/RunE, cobra answers
// an unknown subcommand with the group's help and exit 0 (kb gotcha
// 6ed9b0b3), so a script calling a removed subcommand would "succeed".
func TestIdentity_UnknownSubcommandExitsNonZero(t *testing.T) {
	out, err := run(t, "", "identity")
	if err != nil || !strings.Contains(out, "show") {
		t.Fatalf("fixture: bare `identity` should print its help and succeed: %v\n%s", err, out)
	}
	// The verbs every fleet operation replaced with a REST route are gone,
	// and calling one fails (a script does not "succeed" on a removed verb).
	for _, gone := range []string{"init-root", "enroll", "install", "revoke", "root-fingerprint", "import-root"} {
		root := RootCmd()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"identity", gone, "x"})
		err = Execute(context.Background(), root)
		if err == nil || !strings.Contains(err.Error(), `unknown command "`+gone+`"`) {
			t.Fatalf("identity %s x: %v, want an unknown-command error (a non-zero exit)", gone, err)
		}
	}
}

func TestGrants_AddListRevokeAndTheRefusals(t *testing.T) {
	home := t.TempDir()
	useHome(t, home)
	if _, err := run(t, "", "grants", "list"); err == nil || !strings.Contains(err.Error(), "knomit serve") {
		t.Fatalf("grants on an unserved home: %v", err)
	}
	reg, err := control.OpenRegistry(filepath.Join(home, "control.db")) // what the first `knomit serve` does
	if err != nil {
		t.Fatal(err)
	}
	reg.Close()

	fp := strings.Repeat("ab", 32)
	inst := "instance:" + fp + "@cert"
	if out, err := run(t, "", "grants", "add", inst, "write", "--by", "op"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	out, err := run(t, "", "grants", "list", inst)
	if err != nil || !strings.Contains(out, inst+"\twrite\tlive\tby=op") {
		t.Fatalf("list after add: %v\n%s", err, out)
	}

	// revoke read on an instance: refused, by PARSING, with the explanation.
	_, err = run(t, "", "grants", "revoke", inst, "read")
	if err == nil || !strings.Contains(err.Error(), "POST /api/v1/fleet/revoke") {
		t.Fatalf("revoke read on an instance: %v", err)
	}
	// Positive control: read on a non-instance principal is revocable.
	if _, err := run(t, "", "grants", "revoke", "operator:"+fp+"@cert", "read"); err != nil {
		t.Fatalf("revoke read on an operator: %v", err)
	}

	for _, bad := range [][]string{
		{"grants", "add", "anonymous@none", "write"},                            // anonymous comes from config
		{"grants", "add", "agent:x@cert", "write"},                              // unknown kind
		{"grants", "add", inst, "wirte"},                                        // unknown permission
		{"grants", "add", "instance@cert", "write"},                             // no id
		{"grants", "add", "instance:" + fp[:8] + "@cert", "write"},              // the 8-hex short form
		{"grants", "add", "instance:" + strings.ToUpper(fp) + "@cert", "write"}, // not lowercase
	} {
		if _, err := run(t, "", bad...); err == nil {
			t.Fatalf("%v succeeded", bad)
		}
	}

	if _, err := run(t, "", "grants", "revoke", inst, "write"); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "", "grants", "list")
	if !strings.Contains(out, inst+"\twrite\trevoked ") {
		t.Fatalf("list after revoke:\n%s", out)
	}
}

// show prints the agent id the instance persisted in control.db — the one
// app.New resolves and authors commits as — not anything built from the key.
// The stored id here deliberately shares nothing with the key's fingerprint,
// so a show that re-derived <host>-<fp8> would print a different id.
func TestIdentityShow_PrintsPersistedAgentID(t *testing.T) {
	home, pub, _ := instanceHome(t, "laptop")
	const stored = "opaque-agent-0001"
	if _, err := repos.ResolveIdentity(home, repos.Identity{AgentID: stored, Branch: "agent/" + stored, Host: "laptop"}); err != nil {
		t.Fatal(err)
	}
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "agent_id: "+stored+"\n") {
		t.Fatalf("show does not print the persisted agent id %q:\n%s", stored, out)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	h := sha256.Sum256(sshPub.Marshal())
	if fp8 := hex.EncodeToString(h[:])[:8]; strings.Contains(out, "agent_id: laptop-"+fp8) {
		t.Fatalf("show printed an id derived from the key:\n%s", out)
	}
}

// With no control.db yet (the instance never served), show says there is no
// agent id and does NOT create control.db as a side effect.
func TestIdentityShow_NoAgentIDBeforeFirstBoot(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, "control.db")); !os.IsNotExist(err) {
		t.Fatalf("show created control.db (stat err %v)", err)
	}
	if !strings.Contains(out, "agent_id: none") {
		t.Fatalf("show without control.db:\n%s", out)
	}
}

// seedControlDB writes home/control.db holding exactly the given DDL/DML and
// nothing else — no repos table, no schema_migrations — so a test can tell
// afterwards whether anything migrated it.
func seedControlDB(t *testing.T, home string, stmts ...string) string {
	t.Helper()
	path := filepath.Join(home, "control.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	return path
}

// controlTables lists the tables in the control.db at path.
func controlTables(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

const seedInstanceIdentityTable = `CREATE TABLE instance_identity (
	id INTEGER PRIMARY KEY CHECK (id = 1), agent_id TEXT NOT NULL, branch TEXT NOT NULL,
	created_at INTEGER NOT NULL)`

// show must read the agent id WITHOUT migrating control.db: a newer CLI must
// not run controlUp (lens re-key, migrate.Control) behind the back of an
// instance that last booted, or is running, on an older binary. The seeded
// file holds only instance_identity; after show it must hold only that still.
func TestIdentityShow_DoesNotMigrateControlDB(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	const stored = "opaque-agent-unmigrated"
	path := seedControlDB(t, home, seedInstanceIdentityTable,
		`INSERT INTO instance_identity (id, agent_id, branch, created_at) VALUES (1, '`+stored+`', 'agent/`+stored+`', 1)`)
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "agent_id: "+stored+"\n") {
		t.Fatalf("show does not print the stored agent id %q:\n%s", stored, out)
	}
	if got := strings.Join(controlTables(t, path), ","); got != "instance_identity" {
		t.Fatalf("show changed control.db's tables to [%s], want only [instance_identity] (no repos, no schema_migrations)", got)
	}
}

// A home last booted by a binary from before migration 000012 has a
// control.db with no instance_identity table: show says there is no id yet,
// rather than "unreadable: no such table", and creates no table.
func TestIdentityShow_NoInstanceIdentityTable(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	path := seedControlDB(t, home, `CREATE TABLE pre_000012 (x INTEGER)`)
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "agent_id: none (assigned when the instance first starts)\nkey: ") {
		t.Fatalf("show with no instance_identity table:\n%s", out)
	}
	if got := strings.Join(controlTables(t, path), ","); got != "pre_000012" {
		t.Fatalf("show changed control.db's tables to [%s]", got)
	}
}

// show reads through the read-only opener, not merely a non-migrating one.
// A read-write open (OpenRegistryNoSchema) of a rollback-journal control.db
// persistently switches it to WAL — header bytes 18-19 go from 1,1 to 2,2 —
// so a show that still reports 1,1 afterwards never opened it for writing.
func TestIdentityShow_OpensControlDBReadOnly(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	const stored = "opaque-agent-readonly"
	path := seedControlDB(t, home, seedInstanceIdentityTable,
		`INSERT INTO instance_identity (id, agent_id, branch, created_at) VALUES (1, '`+stored+`', 'agent/`+stored+`', 1)`)
	header := func() [2]byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 20 {
			t.Fatalf("control.db is %d bytes, no SQLite header", len(b))
		}
		return [2]byte{b[18], b[19]}
	}
	if got := header(); got != [2]byte{1, 1} {
		t.Fatalf("seeded control.db header bytes 18-19 = %v, want [1 1] (rollback journal)", got)
	}
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "agent_id: "+stored+"\n") {
		t.Fatalf("show does not print the stored agent id %q:\n%s", stored, out)
	}
	if got := header(); got != [2]byte{1, 1} {
		t.Fatalf("show changed control.db header bytes 18-19 to %v, want [1 1]: it opened control.db read-write", got)
	}
}

// instance_identity exists but holds no row (a boot that failed between the
// migration and the identity insert): show says there is no id yet.
func TestIdentityShow_NoInstanceIdentityRow(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	seedControlDB(t, home, seedInstanceIdentityTable)
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "agent_id: none (assigned when the instance first starts)\nkey: ") {
		t.Fatalf("show with an empty instance_identity:\n%s", out)
	}
}

// A stored agent id that is no longer valid (one holding an upper-case
// letter, minted before agent ids were lower case) is one boot refuses with
// repos.ErrStoredAgentIDInvalid. show must still succeed, print the id AS
// STORED, say it is invalid and that a fresh start is needed, and leave
// control.db byte-for-byte unchanged: nothing rewrites a stored id.
func TestIdentityShow_InvalidStoredAgentID(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	const stored = "Opaque-Agent-UPPER"
	path := seedControlDB(t, home, seedInstanceIdentityTable,
		`INSERT INTO instance_identity (id, agent_id, branch, created_at) VALUES (1, '`+stored+`', 'agent/`+stored+`', 1)`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show with an invalid stored agent id failed: %v\n%s", err, out)
	}
	first, rest, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(first, "agent_id: "+stored+" (invalid: ") {
		t.Fatalf("show's first line does not print the stored id as invalid: %q", first)
	}
	if !strings.Contains(first, "upper-case") || !strings.Contains(first, "fresh start") {
		t.Fatalf("show's invalid-id line names neither the reason nor the remedy: %q", first)
	}
	if !strings.HasPrefix(rest, "key: ") {
		t.Fatalf("show stopped after the invalid agent id:\n%s", out)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("show changed control.db while reporting an invalid stored agent id")
	}
}
