package serveraddr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// The reload CLIENT: how a process of the same user asks a running `knomit
// serve` to re-read its configuration, and reads the answer back from the
// server's own server.json. It is the single implementation behind
// `knomit reload` (cmd/reload.go) and the desktop Settings window
// (tools/desktop/native.go), so the two cannot disagree about what counts as
// acknowledged. There is deliberately no HTTP route for it (kb invariant
// desktop/fleet/install-surface): the trigger is a SIGHUP or, on Windows, a
// session-local event, delivered by the OS to same-user processes only.

// ReloadRefusedError: the server could not load or validate the new
// configuration, so it kept the running one (generation unchanged).
type ReloadRefusedError struct{ Message string }

func (e *ReloadRefusedError) Error() string {
	return "reload refused, the running configuration is unchanged: " + e.Message
}

// ReloadTimeoutError: the signal was delivered but the server did not
// acknowledge within Wait.
type ReloadTimeoutError struct {
	Wait       time.Duration
	Generation int
}

func (e *ReloadTimeoutError) Error() string {
	return fmt.Sprintf("reload requested, but the server did not acknowledge within %s (generation still %d); "+
		"its log says why if the configuration did not load", e.Wait, e.Generation)
}

// ReloadListenerError: the server applied the reload (generation advanced)
// but at least one listener is not as configured. Info is the server.json
// that says which, and why.
type ReloadListenerError struct {
	Info   LockInfo
	Failed []string // "<listener>: <reason>", sorted
}

func (e *ReloadListenerError) Error() string {
	return fmt.Sprintf("reloaded, but not every listener is as configured (%s); the previous listener keeps serving where there was one", strings.Join(e.Failed, ", "))
}

// TLSWaitingForCertificate is what `knomit reload` prints for a fleet
// listener that is configured but waits for this instance's certificate
// (reason no_certificate). Wording approved by the user (2026-10-17, PR
// #545).
const TLSWaitingForCertificate = "waits for this instance's certificate: it starts when this instance creates or joins a fleet"

// ListenerWaitsForCertificate reports whether l is a fleet listener that is
// configured before this instance has a certificate and that nothing serves
// in the meantime: reason no_certificate AND nothing listening. That is the
// fresh-instance path (set the Fleet port, then create or join a fleet), and
// the create or join that installs the certificate starts it
// (app.Listeners.StartTLSWhenInstalled). The same reason with a listener
// still bound is NOT waiting: a reload asked for a change (a new address or
// directory with no certificate in it) that did not happen, and the old
// listener keeps serving.
func ListenerWaitsForCertificate(l LockListener) bool {
	return l.Reason == "no_certificate" && l.Listening == ""
}

// ReloadListenerFailures lists the listeners whose reason says a requested
// change did not happen. Two states are not failures: "off" is a state the
// user asked for, and a listener waiting for its certificate
// (ListenerWaitsForCertificate); treating that as failed made the step
// before the create look broken.
func ReloadListenerFailures(cur LockInfo) []string {
	var bad []string
	for n, l := range cur.Listeners {
		if l.Reason != "" && l.Reason != "off" && !ListenerWaitsForCertificate(l) {
			bad = append(bad, n+": "+l.Reason)
		}
	}
	sort.Strings(bad)
	return bad
}

// RequestReload asks the server of home to reload and, when wait > 0, waits
// for its acknowledgement: server.json's generation advancing (nil, or a
// *ReloadListenerError when a listener is not as configured; the LockInfo is
// returned either way), or a new reload_error (*ReloadRefusedError), or the
// wait running out (*ReloadTimeoutError). With wait <= 0 it returns right
// after signalling, with the LockInfo as read before.
//
// It signals nothing but a server that says it listens (server.json
// "reload": true) and whose local listener answers: SIGHUP's default action
// ends a process, so a stale server.json whose pid was reused, or a server
// from before reload existed, must never be signalled.
func RequestReload(ctx context.Context, home string, wait time.Duration) (LockInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := config.LockfilePathFor(home)
	info, err := ReadLock(path)
	if errors.Is(err, os.ErrNotExist) {
		return LockInfo{}, fmt.Errorf("no running knomit serve for %s (no %s)", home, path)
	}
	if err != nil {
		return LockInfo{}, err
	}
	if info.Home != "" && info.Home != home {
		return info, fmt.Errorf("server.json at %s belongs to the data root %s, not %s", path, info.Home, home)
	}
	if !info.Reload {
		return info, fmt.Errorf("the server (pid %d, %s) does not listen for reload: restart it to apply configuration changes", info.PID, info.Version)
	}
	if info.Socket != "" {
		// The pid in the file is only as good as the file: ask the listener
		// that file names whether anything is there, before signalling.
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, derr := auth.DialLocal(dctx, info.Socket, 2*time.Second)
		cancel()
		if derr != nil {
			return info, fmt.Errorf("server.json names pid %d, but its local listener does not answer (%v); not signalling it", info.PID, derr)
		}
		conn.Close()
	}
	if err := SignalReload(home, info.PID); err != nil {
		return info, err
	}
	if wait <= 0 {
		return info, nil
	}
	deadline := time.Now().Add(wait)
	for {
		cur, rerr := ReadLock(path)
		if rerr == nil && cur.PID == info.PID && cur.Generation > info.Generation {
			if bad := ReloadListenerFailures(cur); len(bad) > 0 {
				return cur, &ReloadListenerError{Info: cur, Failed: bad}
			}
			return cur, nil
		}
		if rerr == nil && cur.PID == info.PID && cur.ReloadError != "" && cur.ReloadError != info.ReloadError {
			// The configuration did not load: nothing changed, generation too.
			return cur, &ReloadRefusedError{Message: cur.ReloadError}
		}
		if time.Now().After(deadline) {
			return info, &ReloadTimeoutError{Wait: wait, Generation: info.Generation}
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// missingEventError is the answer on Windows when the reload event does not
// exist. Local\ objects are per logon session, so if server.json names a pid
// that is alive the likeliest cause is that the server runs in a different
// Windows session (a service, another user's session, an RDP session) than
// this process; only a dead pid means "no server". Spelled here, not in the
// Windows file, so it is testable anywhere.
func missingEventError(pid int, alive bool) error {
	if alive {
		return fmt.Errorf("the reload event does not exist, but server.json names pid %d, which is running: "+
			"the server is probably running in a different Windows session (the event is session-local); "+
			"run knomit reload from that session, or restart the server", pid)
	}
	return errors.New("no running knomit serve found for this data root (its reload event does not exist and the pid in server.json is not running)")
}
