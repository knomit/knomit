package fact

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidExpires is returned by SerializeFact for an `expires` value that is
// not an RFC 3339 timestamp.
var ErrInvalidExpires = errors.New("invalid expires")

// Expires is an optional RFC 3339 timestamp in a fact's frontmatter. Past it
// the fact is EXPIRED, and that is all the word means: knomit never deletes,
// hides, or retracts anything because of it. Readers search for expired facts
// and decide. Absent means never; there is no default anywhere.
//
// The field is a STRING, not a time.Time, on purpose: yaml.v3 decodes a
// date-only value ("2026-10-01") into time.Time without error, so a time.Time
// field would accept exactly the ambiguous input this validation refuses. The
// value is kept as written (offset and all) so a round trip is byte-stable.

// validateExpires is the write-side rule: empty, or a full RFC 3339 timestamp
// with an explicit offset. Date-only and naive local times are refused.
func validateExpires(s string) error {
	if s == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return fmt.Errorf("%w %q: want an RFC 3339 timestamp with an offset, e.g. 2026-10-01T00:00:00Z", ErrInvalidExpires, s)
	}
	return nil
}

// ValidateExpires exposes the write-side rule to callers that check input
// before building a fact (MCP and REST handlers).
func ValidateExpires(s string) error { return validateExpires(s) }

// ExpiresAt returns the parsed expiry. ok is false when the fact has none.
func (f Fact) ExpiresAt() (t time.Time, ok bool) {
	if f.Expires == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, f.Expires)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// IsExpired reports whether the fact's expiry is at or before now. A fact with
// no expiry is never expired.
//
// Compared at WHOLE-SECOND granularity, exactly as the index compares
// facts.expires_at (unix seconds) in the search filter — so the `expired`
// marker on a result and the `expired` filter that selected it can never
// disagree about a sub-second expiry.
func (f Fact) IsExpired(now time.Time) bool { return IsExpiredAt(f.Expires, now) }

// IsExpiredAt is IsExpired for a bare value, for result rows that carry the
// string rather than a Fact.
func IsExpiredAt(expires string, now time.Time) bool {
	u := ExpiresUnix(expires)
	return u != nil && *u <= now.Unix()
}

// ExpiresUnix returns the expiry as unix seconds for the index column, or nil
// when absent or unparseable.
func ExpiresUnix(s string) *int64 {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	u := t.Unix()
	return &u
}
