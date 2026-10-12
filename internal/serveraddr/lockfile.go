// The server discovery file (server.json): what a running server tells the
// processes that look for it (kb, hooks, the desktop) about itself -- its own
// address, so it lives beside the one spelling of that address.
//
// WHICH file is a question for internal/config (LockfilePath, LockfilePathFor):
// the default home's file lives in the per-user state directory, any other
// home's lives in that home, so two homes cannot overwrite each other's.
package serveraddr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLockCorrupt wraps a lockfile that exists but cannot be parsed. Callers
// distinguish it from a genuine I/O error (e.g. permission denied): a corrupt
// lockfile records no usable instance and is safe to overwrite, whereas an I/O
// error is a real problem the caller should surface.
var ErrLockCorrupt = errors.New("corrupt lockfile")

// LockSchemaV2 is LockInfo.Schema of a file written by `knomit serve`.
const LockSchemaV2 = 2

// LockInfo is the file's content. Schema 1 was {pid, port, version} and its
// readers (older kb builds) still work on a schema 2 file: Port keeps its
// meaning and is 0 when the server has no TCP listener, which they read as
// "use the default address", i.e. the local socket first.
//
// Fields below Version are schema 2. They are informative to people and
// diagnostics, except HTTP, which a reader takes in preference to Port.
// None of them names the socket kb dials: kb derives that itself from
// KNOMIT_HOME (config.SocketPath), because a file that can be shared between
// homes must not decide which instance a client talks to.
type LockInfo struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"` // plaintext TCP port; 0 = the server has no TCP listener
	Version string `json:"version"`

	Schema int    `json:"schema,omitempty"` // LockSchemaV2 for files written by `knomit serve`
	Home   string `json:"home,omitempty"`   // the data root this server serves
	// HTTP is the plaintext listener's full base URL ("http://127.0.0.1:19278"),
	// "" when the server runs with http = false. Always written, so an off
	// listener is distinguishable from a schema 1 file.
	HTTP string `json:"http"`
	// Address is the server's own address in KNOMIT_SERVER's spelling: its
	// local socket/pipe when that bound, else its TCP address.
	Address string `json:"address,omitempty"`
	// Socket is the local listener's path when it bound, "" when it did not.
	Socket string `json:"socket,omitempty"`
	// TLS is the mTLS listener's bound address, "" when it is not listening.
	TLS string `json:"tls,omitempty"`
	// Generation counts listener-set applications by this process; 1 after boot.
	Generation int `json:"generation,omitempty"`
	// Reload is true when this process listens for a reload request (SIGHUP,
	// or the Windows event). A server written before reload existed lacks it,
	// and SIGHUP would end it, so `knomit reload` refuses to signal one.
	Reload bool `json:"reload,omitempty"`
	// ReloadError is "<RFC3339 time>: <message>" when the last reload request
	// could not load or validate the configuration (the running configuration
	// and Generation are unchanged); cleared by the next successful reload.
	// The time makes a repeated identical error a new value.
	ReloadError string `json:"reload_error,omitempty"`
	// RestartNeeded names the config keys whose change the last reload saw
	// and did NOT apply (socket, [auth].require, ...): the running process
	// still has the old values until it is restarted. Empty after a boot.
	RestartNeeded []string `json:"restart_needed,omitempty"`
	// Listeners is, per reloadable listener ("http", "tls"), what the last
	// boot or reload did with it, so a caller that signalled a reload can read
	// the outcome (a held port) from the same file whose Generation it polls.
	Listeners map[string]LockListener `json:"listeners,omitempty"`
}

// LockListener is one listener's outcome: the address as configured ("" =
// off), the address bound ("" = not listening) and why they differ
// ("addr_in_use", "no_certificate", "off", or the bind error of a reload that
// kept the previous listener).
type LockListener struct {
	Configured string `json:"configured"`
	Listening  string `json:"listening"`
	Reason     string `json:"reason,omitempty"`
}

// WriteLock serializes info to path atomically (write-then-rename) with 0600 perms.
// Creates parent directories as needed.
func WriteLock(path string, info LockInfo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lockfile: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "server-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create tempfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename tempfile: %w", err)
	}
	return nil
}

// ReadLock returns the LockInfo recorded in the lockfile at path.
// If the file does not exist, returns an error wrapping os.ErrNotExist.
func ReadLock(path string) (LockInfo, error) {
	var info LockInfo
	data, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, fmt.Errorf("%w: parse %s: %v", ErrLockCorrupt, path, err)
	}
	return info, nil
}

// RemoveLock deletes the lockfile at path. Missing file is not an error.
func RemoveLock(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveLockIfOwner deletes the lockfile at path only when it still records pid.
// A server stopping must not delete the file of a successor (or of a sibling
// that overwrote it) that has since written its own. A missing or unreadable
// file is left alone and is not an error.
func RemoveLockIfOwner(path string, pid int) error {
	info, err := ReadLock(path)
	if err != nil || info.PID != pid {
		return nil
	}
	return RemoveLock(path)
}
