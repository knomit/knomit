// Package sessions owns the client_sessions table in control.db: who is
// connected over MCP, declared metadata, and last-seen liveness. It is
// OPERATIONAL state, never knowledge — nothing here is written as a fact.
//
// See .claude/plans/2026-09-14-client-session-tracking-design.md and the
// knomit decisions under kb/decisions/mcp/client-sessions/.
package sessions

import (
	"fmt"
	"time"
)

// Policy holds the presence thresholds. All three are operator settings
// ([session] client_* in config); none is a corpus property.
type Policy struct {
	// DeadAfter: silent longer than this ⇒ StateDead.
	DeadAfter time.Duration
	// HiddenAfter: silent longer than this ⇒ excluded from the presence view.
	HiddenAfter time.Duration
	// Retention: rows with last_seen_at older than this are purged. 0 = never.
	Retention time.Duration
}

// Threshold defaults, mirrored by config.Defaults(). Kept here too so the
// package is usable (and testable) without a loaded config.
const (
	DefaultDeadAfter   = time.Hour
	DefaultHiddenAfter = 3 * time.Hour
	DefaultRetention   = 168 * time.Hour
)

// ParsePolicy parses the raw config strings. Empty ⇒ default. "0" is a
// legitimate value ONLY for retention (disables purge); for dead/hidden a
// non-positive value falls back to the default, since a zero window would
// mark every session dead the moment it is recorded.
func ParsePolicy(dead, hidden, retention string) (Policy, error) {
	parse := func(field, s string, def time.Duration) (time.Duration, error) {
		if s == "" {
			return def, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("session.%s: %w", field, err)
		}
		return d, nil
	}
	d, err := parse("client_dead_after", dead, DefaultDeadAfter)
	if err != nil {
		return Policy{}, err
	}
	h, err := parse("client_hidden_after", hidden, DefaultHiddenAfter)
	if err != nil {
		return Policy{}, err
	}
	r, err := parse("client_retention", retention, DefaultRetention)
	if err != nil {
		return Policy{}, err
	}
	if d <= 0 {
		d = DefaultDeadAfter
	}
	if h <= 0 {
		h = DefaultHiddenAfter
	}
	if r < 0 {
		r = 0
	}
	return Policy{DeadAfter: d, HiddenAfter: h, Retention: r}, nil
}

// State is the read-time presence classification of a session.
type State string

// The three presence states. Nothing writes these: they are derived from
// last_seen_at, ended_at and the policy at read time.
const (
	StateLive State = "live"
	StateIdle State = "idle"
	StateDead State = "dead"
)

// LiveWindow is how recently a session must have been seen to count as
// live: a tenth of DeadAfter (6 minutes at defaults). Derived, not a
// fourth setting.
func (p Policy) LiveWindow() time.Duration { return p.DeadAfter / 10 }

// StateAt classifies a session from its last activity. ended is true when
// the client sent an explicit session termination.
func (p Policy) StateAt(lastSeen time.Time, ended bool, now time.Time) State {
	if ended {
		return StateDead
	}
	silent := now.Sub(lastSeen)
	switch {
	case silent <= p.LiveWindow():
		return StateLive
	case silent <= p.DeadAfter:
		return StateIdle
	default:
		return StateDead
	}
}

// HiddenAt reports whether a session silent since lastSeen is past the
// presence window.
func (p Policy) HiddenAt(lastSeen, now time.Time) bool {
	return now.Sub(lastSeen) > p.HiddenAfter
}
