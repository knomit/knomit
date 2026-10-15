package repos

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"knomit/internal/pki"
	"knomit/internal/platform/fileuri"
)

// Identity is this instance's persisted identity (control.db
// instance_identity): its opaque agent id (the commit author), its agent
// branch, and the host label it was created with. The three are written
// TOGETHER, once, and read back as stored ever after. Nothing derives one of
// them from another by parsing a name (D18): the id is not "branch minus
// agent/", and the host is not "id minus its fingerprint".
type Identity struct {
	AgentID string
	Branch  string
	// Host is the sanitized hostname at creation, a label ({host} in a
	// trigger match). Empty for an identity created before it was stored.
	Host string
}

// ResolveIdentity returns this instance's identity, persisted in control.db
// (F09 PR 5, D1).
//
// Before PR 5 the branch was re-derived at every boot from the current key and
// hostname, so a key rotation or a hostname change gave the instance a new id.
// Now fresh is used ONCE: on the first boot that finds no instance_identity
// row, it is written, and every later boot returns the stored identity
// whatever the key or hostname. fresh is BUILT by the caller (app constructs
// id and branch from the host and the key's fp8); it is never parsed back.
//
// control.db is opened through OpenRegistry, the one path allowed to migrate
// it, and closed again: the Manager opens its own handle at Start.
func ResolveIdentity(home string, fresh Identity) (Identity, error) {
	reg, err := OpenRegistry(filepath.Join(home, "control.db"))
	if err != nil {
		return Identity{}, fmt.Errorf("instance identity: %w", err)
	}
	defer reg.Close()
	return resolveIdentity(reg.DB(), fresh, time.Now())
}

// ErrStoredAgentIDInvalid: the agent id persisted in control.db is not a
// valid agent id (pki.ValidAgentID: an id holding an upper-case letter, from
// before agent ids were minted lower case, is the expected case). Boot
// refuses it; nothing rewrites a stored id. The remedy is a fresh start:
// stop knomit, delete control.db, and start again — the instance mints a new
// lower-case id and registers again (kb/decisions/identity/opaque-persisted-agent-id).
var ErrStoredAgentIDInvalid = errors.New("instance identity: the stored agent id is invalid; a fresh start is required (stop knomit, delete control.db in the knomit home, and start again: the instance mints a new lower-case agent id and must register with its fleet again)")

// StoredAgentID returns the agent id persisted in home's control.db — the id
// app.New resolves at boot and every commit is authored as — for display
// (`knomit identity show`). It never creates or migrates control.db: the file
// is opened read-only (openControlReadOnly), so neither is possible whatever
// state the file is in. found is false when control.db, its instance_identity
// table (a home last booted before migration 000012) or its row does not exist
// yet. The id is returned as stored; it is opaque and nothing may parse it
// (D18).
//
// It deliberately does NOT go through controlUp: a display command run by a
// newer CLI must not migrate the control.db of an instance that last booted,
// or is still running, on an older binary.
func StoredAgentID(home string) (id string, found bool, err error) {
	path := filepath.Join(home, "control.db")
	// Only chooses the "none" answer; the read-only open is what keeps a
	// missing control.db from being created.
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("instance identity: %w", err)
	}
	db, err := openControlReadOnly(path)
	if err != nil {
		return "", false, fmt.Errorf("instance identity: %w", err)
	}
	defer db.Close()
	if ok, err := tableExists(db, "instance_identity"); err != nil || !ok {
		return "", false, err
	}
	err = db.QueryRow(`SELECT agent_id FROM instance_identity WHERE id = 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("instance identity: read: %w", err)
	}
	return id, true, nil
}

// openControlReadOnly opens control.db at path with SQLite's mode=ro: the
// handle can neither create a missing file nor write — and so never migrate —
// an existing one. A missing file surfaces as an error on first use.
//
// path is made absolute first: fileuri.New keeps a relative path relative,
// and go-sqlite3 DROPS the query string of a DSN that does not start with
// "file:", so mode=ro would silently become the default read-write-create.
func openControlReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("open control.db read-only: %w", err)
	}
	db, err := sql.Open("sqlite3", controlReadOnlyDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("open control.db read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// controlReadOnlyDSN is the SQLite URI for an absolute control.db path. It
// goes through fileuri.New because a hand-built url.URL{Scheme: "file",
// Path: ...} renders a Windows drive path as file://C:/..., where "C:" is the
// URI authority and SQLite rejects it ("invalid uri authority: C:").
func controlReadOnlyDSN(absPath string) string {
	return fileuri.New(absPath) + "?mode=ro&_busy_timeout=5000"
}

func resolveIdentity(db *sql.DB, fresh Identity, now time.Time) (Identity, error) {
	var id Identity
	var host sql.NullString
	err := db.QueryRow(`SELECT i.agent_id, i.branch, h.host FROM instance_identity i
		LEFT JOIN instance_identity_host h ON h.id = i.id WHERE i.id = 1`).Scan(&id.AgentID, &id.Branch, &host)
	if err == nil {
		// The stored id must still be one this binary would mint: lower
		// case, reading back as itself. There is no legacy path and no
		// rewrite: an id minted before the rule is a fresh start.
		if verr := pki.ValidAgentID(id.AgentID); verr != nil {
			return Identity{}, fmt.Errorf("%w: control.db holds agent id %q: %v", ErrStoredAgentIDInvalid, id.AgentID, verr)
		}
		id.Host = host.String
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Identity{}, fmt.Errorf("instance identity: read: %w", err)
	}
	if fresh.AgentID == "" || fresh.Branch == "" {
		return Identity{}, errors.New("instance identity: no stored identity and none to create (empty agent id or branch)")
	}
	// The id is minted HERE, once. It must read back as itself from the
	// address it commits as and its certificate carries,
	// <id>@agents.knomit.io: an id holding '+' would be read as <before +>,
	// and judged as another agent (R2).
	if err := pki.ValidAgentID(fresh.AgentID); err != nil {
		return Identity{}, fmt.Errorf("instance identity: refusing to mint agent id: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return Identity{}, fmt.Errorf("instance identity: write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO instance_identity (id, agent_id, branch, created_at, fleet_state_since)
		VALUES (1, ?, ?, ?, ?)`, fresh.AgentID, fresh.Branch, now.Unix(), now.Unix()); err != nil {
		return Identity{}, fmt.Errorf("instance identity: write: %w", err)
	}
	if fresh.Host != "" {
		if _, err := tx.Exec(`INSERT INTO instance_identity_host (id, host) VALUES (1, ?)`, fresh.Host); err != nil {
			return Identity{}, fmt.Errorf("instance identity: write host: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Identity{}, fmt.Errorf("instance identity: write: %w", err)
	}
	return fresh, nil
}

// Fleet membership states (instance_identity.fleet_state).
const (
	FleetStandalone    = "standalone"
	FleetRegistering   = "registering"
	FleetRegistered    = "registered"
	FleetUnregistering = "unregistering"
)

// FleetRow is this instance's persisted identity and fleet state.
type FleetRow struct {
	AgentID      string
	Branch       string
	State        string
	Since        time.Time
	RegisteredAt time.Time // zero when never registered
	LastAttempt  time.Time // zero when no push or fetch was attempted
	LastError    string    // the latest push/fetch failure, "" after a success
}

func unixOrZero(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

func readFleetRow(db *sql.DB) (FleetRow, error) {
	var r FleetRow
	var since, reg, attempt sql.NullInt64
	var lastErr sql.NullString
	err := db.QueryRow(`SELECT agent_id, branch, fleet_state, fleet_state_since, fleet_registered_at,
		fleet_last_attempt, fleet_last_error FROM instance_identity WHERE id = 1`).
		Scan(&r.AgentID, &r.Branch, &r.State, &since, &reg, &attempt, &lastErr)
	if err != nil {
		return FleetRow{}, fmt.Errorf("fleet state: %w", err)
	}
	r.Since, r.RegisteredAt, r.LastAttempt = unixOrZero(since), unixOrZero(reg), unixOrZero(attempt)
	r.LastError = lastErr.String
	return r, nil
}

// sqlExecer is a *sql.DB or a *sql.Tx.
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// setFleetState moves the state machine. Entering registered stamps
// registered_at; entering standalone clears it and the last error.
func setFleetState(db sqlExecer, state string, now time.Time) error {
	q := `UPDATE instance_identity SET fleet_state = ?, fleet_state_since = ? WHERE id = 1`
	args := []any{state, now.Unix()}
	switch state {
	case FleetRegistered:
		q = `UPDATE instance_identity SET fleet_state = ?, fleet_state_since = ?, fleet_registered_at = ? WHERE id = 1`
		args = append(args, now.Unix())
	case FleetStandalone:
		q = `UPDATE instance_identity SET fleet_state = ?, fleet_state_since = ?, fleet_registered_at = NULL,
			fleet_last_error = NULL WHERE id = 1`
	}
	res, err := db.Exec(q, args...)
	if err != nil {
		return fmt.Errorf("fleet state: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("fleet state: no instance identity row")
	}
	return nil
}

// recordFleetAttempt records one push/fetch attempt: a failure sets
// last_error, a success clears it.
func recordFleetAttempt(db *sql.DB, attemptErr error, now time.Time) error {
	var msg any
	if attemptErr != nil {
		msg = attemptErr.Error()
	}
	_, err := db.Exec(`UPDATE instance_identity SET fleet_last_attempt = ?, fleet_last_error = ? WHERE id = 1`, now.Unix(), msg)
	return err
}

// recordFleetRepo records uid as this instance's fleet repository (control.db
// instance_fleet_repo, migration 000016), replacing any earlier one. The
// fleet code calls it when it mounts the fleet repository; boot admits a
// fleet-id ontology only for the recorded uid (fleet_chokepoint.go). Purging
// the repository clears it (the foreign key cascades).
func recordFleetRepo(db *sql.DB, uid string) error {
	if _, err := db.Exec(`INSERT INTO instance_fleet_repo (id, repo_uid) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET repo_uid = excluded.repo_uid`, uid); err != nil {
		return fmt.Errorf("fleet repository: record: %w", err)
	}
	return nil
}

// recordedFleetRepo is the uid recordFleetRepo stored; "" when none is.
func recordedFleetRepo(db *sql.DB) (string, error) {
	var uid string
	err := db.QueryRow(`SELECT repo_uid FROM instance_fleet_repo WHERE id = 1`).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("fleet repository: read: %w", err)
	}
	return uid, nil
}

// fleetCreateMarker is the row of control.db instance_fleet_create (migration
// 000017): a fleet create that has begun and not finished. CreateFleet
// writes it before its first write and deletes it in the transaction that
// sets the fleet state (finishFleetCreate). A marker found while no create
// runs is a create that died part way, and undoFleetCreate rolls it back.
type fleetCreateMarker struct {
	// RepoName is the fleet repository's local name, free when the create
	// began.
	RepoName  string
	StartedAt time.Time
	// RootID is pki.RootID of the root this create minted, "" until the root
	// exists.
	RootID string
	// TLSDirExisted says whether [tls].dir existed when the create began.
	TLSDirExisted bool
}

// readFleetCreateMarker is the marker, or nil when no create is under way.
func readFleetCreateMarker(db *sql.DB) (*fleetCreateMarker, error) {
	var mk fleetCreateMarker
	var started int64
	err := db.QueryRow(`SELECT repo_name, started_at, root_id, tls_dir_existed FROM instance_fleet_create WHERE id = 1`).
		Scan(&mk.RepoName, &started, &mk.RootID, &mk.TLSDirExisted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fleet create marker: read: %w", err)
	}
	mk.StartedAt = time.Unix(started, 0).UTC()
	return &mk, nil
}

// writeFleetCreateMarker records that a create begins. It fails when a marker
// is already there, because that one must be rolled back first.
func writeFleetCreateMarker(db *sql.DB, mk fleetCreateMarker) error {
	if _, err := db.Exec(`INSERT INTO instance_fleet_create (id, repo_name, started_at, root_id, tls_dir_existed) VALUES (1, ?, ?, '', ?)`,
		mk.RepoName, mk.StartedAt.Unix(), mk.TLSDirExisted); err != nil {
		return fmt.Errorf("fleet create marker: write: %w", err)
	}
	return nil
}

// setFleetCreateRoot records the root the create minted.
func setFleetCreateRoot(db *sql.DB, rootID string) error {
	res, err := db.Exec(`UPDATE instance_fleet_create SET root_id = ? WHERE id = 1`, rootID)
	if err != nil {
		return fmt.Errorf("fleet create marker: record the root: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("fleet create marker: no marker to record the root on")
	}
	return nil
}

// finishFleetCreate ends a create in one transaction: it sets the fleet
// state and deletes the marker. It is a create's point of no return and, with
// state standalone, the last step of its rollback.
func finishFleetCreate(db *sql.DB, state string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("fleet create: finish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := setFleetState(tx, state, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM instance_fleet_create WHERE id = 1`); err != nil {
		return fmt.Errorf("fleet create marker: delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fleet create: finish: %w", err)
	}
	return nil
}
