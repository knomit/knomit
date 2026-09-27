package repos

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"knomit/internal/store"
)

// ResolveIdentity returns this instance's agent branch, persisted in
// control.db (F09 PR 5, D1).
//
// Before PR 5 the branch was re-derived at every boot from the current key and
// hostname (app.agentBranch), so a key rotation or a hostname change gave the
// instance a new id. Now derived is used ONCE: on the first boot that finds no
// instance_identity row, it is written, and every later boot returns the
// stored branch whatever the key or hostname. The commit author follows,
// because it is derived from the branch (store.AgentIDOf).
//
// control.db is opened through OpenRegistry, the one path allowed to migrate
// it, and closed again: the Manager opens its own handle at Start.
func ResolveIdentity(home, derived string) (string, error) {
	reg, err := OpenRegistry(filepath.Join(home, "control.db"))
	if err != nil {
		return "", fmt.Errorf("instance identity: %w", err)
	}
	defer reg.Close()
	return resolveIdentity(reg.DB(), derived, time.Now())
}

func resolveIdentity(db *sql.DB, derived string, now time.Time) (string, error) {
	var branch string
	err := db.QueryRow(`SELECT branch FROM instance_identity WHERE id = 1`).Scan(&branch)
	if err == nil {
		return branch, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("instance identity: read: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO instance_identity (id, agent_id, branch, created_at, fleet_state_since)
		VALUES (1, ?, ?, ?, ?)`, store.AgentIDOf(derived), derived, now.Unix(), now.Unix()); err != nil {
		return "", fmt.Errorf("instance identity: write: %w", err)
	}
	return derived, nil
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

// setFleetState moves the state machine. Entering registered stamps
// registered_at; entering standalone clears it and the last error.
func setFleetState(db *sql.DB, state string, now time.Time) error {
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
