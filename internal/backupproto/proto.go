// Package backupproto defines the newline-delimited JSON protocol spoken
// between knomit (internal/backup, the client) and the knomit-backup agent
// (tools/backup, wrapping internal/backupagent).
//
// # Why there is a protocol at all
//
// litestream v0.5 drives its own SQLite connections through
// modernc.org/sqlite; knomit drives its databases through the cgo
// mattn/go-sqlite3 build, and cannot switch because sqlite-vec has no modernc
// build. Two SQLite BUILDS inside one process do not see each other's locks:
// POSIX advisory record locks never conflict between descriptors held by the
// SAME process, and SQLite's compensating per-process inode table is private to
// one build. Verified by demonstration — knomit's close deleted litestream's
// -wal while litestream held a read lock, and removed the -shm while it was
// mapped, 3 times out of 3.
//
// Across PROCESSES those same POSIX locks work exactly as SQLite intends. That
// is the entire reason litestream now runs in a child process, and this package
// is the seam.
//
// # Shape
//
// One JSON object per line, in both directions. Requests carry a monotonic id;
// responses echo it, so several requests may be in flight at once and a cheap
// `status` never queues behind a slow `restore`.
//
//	--> {"id":1,"method":"track","params":{"name":"core","path":"/data/repos/core.db","rel":"repos/core.db"}}
//	<-- {"id":1,"ok":true}
//	--> {"id":2,"method":"status"}
//	<-- {"id":2,"ok":true,"result":{"databases":[{"name":"core","local_txid":7,"remote_txid":7,"in_sync":true}]}}
//
// Failures are protocol responses, never an exit: the agent stays up and
// answering. Codes exist because knomit's callers branch on error IDENTITY
// (errors.Is(err, ErrDiverged), "is this just an empty replica?"), and an error
// string cannot carry that across a pipe.
//
// The child's stdout carries protocol traffic ONLY. Its logging goes to stderr,
// which the client forwards into knomit's logger.
package backupproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"time"
)

// Methods. Every method is IDEMPOTENT: the client retries across an agent
// restart, and a retry must never mean something different from the first
// attempt. Track/Untrack are no-ops when already in the requested state,
// restores only fill absences, and DeleteReplica deletes an already-empty
// prefix happily.
const (
	MethodOpen            = "open"
	MethodTrack           = "track"
	MethodUntrack         = "untrack"
	MethodStatus          = "status"
	MethodRestore         = "restore"
	MethodPreflight       = "preflight"
	MethodResetLocalState = "reset_local_state"
	MethodDeleteReplica   = "delete_replica"
	MethodClose           = "close"
)

// Error codes. The client maps these back onto the sentinel errors its callers
// already branch on; anything else is an opaque failure carrying only a
// message.
const (
	// CodeNoSnapshot means the replica holds no backup at that prefix. It is a
	// normal first boot, not a failure, and callers must be able to tell it
	// apart from a restore that broke.
	CodeNoSnapshot = "no_snapshot"
	// CodeDiverged means the local database's history does not match the
	// replica's — two writers, or a stale volume. Never auto-recovered.
	CodeDiverged = "diverged"
	// CodeTrackedElsewhere means the name is already replicating a DIFFERENT
	// file. Swallowing that would leave the caller's database backed up by
	// nobody, with no error anywhere.
	CodeTrackedElsewhere = "tracked_elsewhere"
	// CodeNotOpen means a method arrived before a successful open.
	CodeNotOpen = "not_open"
	// CodeBadRequest covers unparseable, oversized, or malformed lines.
	CodeBadRequest = "bad_request"
	// CodeUnknownMethod means the agent is older than the client.
	CodeUnknownMethod = "unknown_method"
	// CodeInternal is everything else.
	CodeInternal = "internal"
)

// Request is one line from knomit to the agent.
type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is one line from the agent to knomit. ID echoes the request's, and
// is 0 for a line the agent could not correlate (a malformed or oversized
// request) — the client logs those rather than delivering them to a waiter.
type Response struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Code   string          `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Config is the replication configuration handed to the agent by open. It
// mirrors knomit's config.BackupConfig minus the enable flag, which is decided
// before the agent is ever spawned.
//
// The durations marshal as integer nanoseconds — Go's default for
// time.Duration. That is machine-friendly rather than pretty, but it is exact
// and needs no parsing rules on either side.
type Config struct {
	URL               string        `json:"url"`
	Instance          string        `json:"instance"`
	SnapshotInterval  time.Duration `json:"snapshot_interval"`
	SnapshotRetention time.Duration `json:"snapshot_retention"`
	L0Retention       time.Duration `json:"l0_retention"`
	MonitorInterval   time.Duration `json:"monitor_interval"`
}

// OpenParams opens the agent's stores and PROBES the replica target, so bad
// credentials or an unreachable bucket fail knomit's boot rather than
// surfacing later as a silent replication stall.
type OpenParams struct {
	Config Config `json:"config"`
}

// TrackParams begins replicating one database.
//
// Rel is supplied by the client rather than derived by the agent: the mapping
// from a logical name to a replica path is knomit's naming policy (control.db,
// repos/<name>.db, archive/<id>.db), and the agent has no business knowing it.
// Archived says which STORE the database belongs to — the archive store runs
// with retention disabled, and that is not a property of the path.
type TrackParams struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Rel      string `json:"rel"`
	Archived bool   `json:"archived,omitempty"`
}

// UntrackParams permanently stops replicating a name.
type UntrackParams struct {
	Name string `json:"name"`
}

// DBStatus is one tracked database's replication state.
type DBStatus struct {
	Name       string `json:"name"`
	LocalTXID  uint64 `json:"local_txid"`
	RemoteTXID uint64 `json:"remote_txid"`
	InSync     bool   `json:"in_sync"`
	LastError  string `json:"last_error,omitempty"`
}

// StatusResult is the status method's payload.
type StatusResult struct {
	Databases []DBStatus `json:"databases"`
}

// RestoreParams restores Rel into Dest, and ONLY when Dest is absent. Dest is
// computed by the client because the layout under KNOMIT_HOME is knomit's, not
// the agent's.
type RestoreParams struct {
	Rel  string `json:"rel"`
	Dest string `json:"dest"`
}

// RestoreResult reports whether anything was written. False with no error means
// the destination already existed; CodeNoSnapshot means the replica had
// nothing.
type RestoreResult struct {
	Restored bool `json:"restored"`
}

// PreflightParams checks an EXISTING local database against its replica.
type PreflightParams struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Rel  string `json:"rel"`
}

// ResetLocalStateParams discards litestream's local LTX state for a database
// file, forcing a re-anchor against the replica on the next open. It is the
// half of Pause/resume that makes a file swap safe.
type ResetLocalStateParams struct {
	Path string `json:"path"`
}

// DeleteReplicaParams permanently removes every object under one replica
// prefix. Scope is exactly that prefix — see backup.DeleteArchivedReplica.
type DeleteReplicaParams struct {
	Rel string `json:"rel"`
}

// MaxLineBytes caps one protocol line. It is generous for the payloads this
// protocol actually carries (the largest is a status listing, a few hundred
// bytes per database) and exists so a corrupt or hostile stream cannot make
// either side buffer without bound.
const MaxLineBytes = 4 << 20 // 4 MiB

// ErrLineTooLong is returned by ReadLine for a line exceeding MaxLineBytes.
// The offending line has been fully consumed by the time it is returned, so
// the caller can report the error and keep reading — an oversized line must
// not wedge the channel.
var ErrLineTooLong = errors.New("backupproto: line exceeds the maximum length")

// ReadLine reads one newline-terminated line, capped at max bytes.
//
// bufio.Scanner is deliberately not used: it STOPS on a too-long token, which
// turns one bad line into a dead channel. This drains the remainder of an
// oversized line so the reader resynchronises on the next newline.
func ReadLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if err == nil {
			if len(buf)+len(chunk) > max {
				return nil, ErrLineTooLong
			}
			return append(buf, chunk...), nil
		}
		if err != bufio.ErrBufferFull {
			if len(buf) == 0 && len(chunk) == 0 {
				return nil, err
			}
			// A final line with no trailing newline: hand it back, and let the
			// next call report the underlying error (EOF).
			return append(buf, chunk...), nil
		}
		if len(buf)+len(chunk) > max {
			// Over the cap. Consume through the newline so the stream stays
			// usable, then report.
			if derr := discardLine(r); derr != nil {
				return nil, derr
			}
			return nil, ErrLineTooLong
		}
		buf = append(buf, chunk...)
	}
}

// discardLine consumes bytes up to and including the next newline.
func discardLine(r *bufio.Reader) error {
	for {
		_, err := r.ReadSlice('\n')
		if err == nil {
			return nil
		}
		if err != bufio.ErrBufferFull {
			return err
		}
	}
}

// idPattern finds an "id": <number> pair anywhere in a line.
//
// A regexp rather than a JSON decode, deliberately: this runs only on lines
// that ALREADY failed to decode — truncated, or with a broken field after the
// id — and a decoder returns nothing for those. Recovering the id is what lets
// the agent answer the waiter that sent the bad line instead of leaving it to
// time out.
var idPattern = regexp.MustCompile(`"id"\s*:\s*(\d+)`)

// PeekID extracts the id from a line that failed to decode as a Request.
// Returns 0 when the id is unrecoverable, which the client logs rather than
// delivering to anyone.
func PeekID(line []byte) uint64 {
	var probe struct {
		ID uint64 `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &probe); err == nil {
		return probe.ID
	}
	m := idPattern.FindSubmatch(line)
	if m == nil {
		return 0
	}
	id, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// WriteLine writes one JSON value followed by a newline. The value is encoded
// in full before anything is written, so a marshal failure cannot leave a
// half-written line on the wire.
func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}
