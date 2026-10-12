//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"

	"knomit/internal/config"
)

// installReloadSignal is Windows' SIGHUP: the server creates the session-local
// event `Local\knomit-reload-<hash of the data root>` (config.ReloadEventName)
// and runs onReload, on its own goroutine and one call at a time, each time
// something sets it. `knomit reload` (and the desktop) set it.
//
// The event is AUTO-RESET, so a set that arrives while a reload runs is kept
// (one, not a queue) and a set before the wait begins is not lost. Its DACL
// grants generic-all to the creating user and to nobody else, owner set to
// that user -- the same shape as the local pipe's, minus SYSTEM: any process
// of the same user can already edit knomit.toml and kill the server, so
// signalling a reload adds no capability, and no other account can open it.
//
// If the name already exists, another live process of this home holds it (a
// second `knomit serve` that lost the socket to the first, or a squatter in
// this session). It is not shared and not opened: this process runs without a
// reload trigger and says so (the error), because a reload that reached the
// OTHER server would be a silent surprise.
//
// The returned func stops the loop and waits for a reload in flight.
func installReloadSignal(home string, onReload func()) (func(), error) {
	name, err := windows.UTF16PtrFromString(config.ReloadEventName(home))
	if err != nil {
		return func() {}, err
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return func() {}, fmt.Errorf("reload event: GetTokenUser: %w", err)
	}
	sid := u.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return func() {}, fmt.Errorf("reload event: security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))

	ev, err := windows.CreateEvent(sa, 0 /* auto-reset */, 0 /* not signalled */, name)
	if err != nil {
		if ev != 0 {
			_ = windows.CloseHandle(ev)
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return func() {}, fmt.Errorf("reload event %s is held by another process", config.ReloadEventName(home))
		}
		return func() {}, fmt.Errorf("reload event: CreateEvent: %w", err)
	}
	stop, err := windows.CreateEvent(nil, 1 /* manual-reset */, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(ev)
		return func() {}, fmt.Errorf("reload event: CreateEvent (stop): %w", err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handles := []windows.Handle{ev, stop}
		for {
			r, werr := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
			if werr != nil {
				log.Error().Err(werr).Msg("reload event: wait failed; reload by event is off")
				return
			}
			switch r {
			case windows.WAIT_OBJECT_0:
				onReload()
			case windows.WAIT_OBJECT_0 + 1:
				return
			default:
				log.Error().Uint32("result", r).Msg("reload event: unexpected wait result; reload by event is off")
				return
			}
		}
	}()
	return func() {
		_ = windows.SetEvent(stop)
		<-finished
		_ = windows.CloseHandle(ev)
		_ = windows.CloseHandle(stop)
	}, nil
}

// signalReload sets the reload event of the server of home. The server's pid
// is not needed (it is for unix); a missing event is "no server".
func signalReload(home string, pid int) error {
	name, err := windows.UTF16PtrFromString(config.ReloadEventName(home))
	if err != nil {
		return err
	}
	ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return missingEventError(pid, processAlive(pid))
		}
		return fmt.Errorf("open the reload event: %w", err)
	}
	defer windows.CloseHandle(ev)
	if err := windows.SetEvent(ev); err != nil {
		return fmt.Errorf("set the reload event: %w", err)
	}
	return nil
}

// processAlive reports whether pid names a running process this user may query.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied still means it exists (another user's process).
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}
