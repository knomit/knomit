//go:build windows

package auth

import (
	"fmt"
	"net"
	"runtime"

	"github.com/Microsoft/go-winio"
	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// ImpersonateNamedPipeClient is not in golang.org/x/sys/windows v0.46, so it
// is resolved by hand. Everything else this file needs (the pid call,
// OpenThreadToken, RevertToSelf) is there.
var procImpersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").
	NewProc("ImpersonateNamedPipeClient")

// PeerCred returns the kernel's view of who is on the other end of a named
// pipe connection — the Windows counterpart of the unix SO_PEERCRED read in
// peercred_unix.go, and the credential for the local bridge here.
//
// The pipe's ACL (auth.ListenLocal's SDDL: this user's SID and SYSTEM, and
// nobody else) is the gate, the way the 0700 data root is on unix. This
// function is what turns "you got through the gate" into a named principal
// rather than an assumption, by asking the OS who the client is.
//
// ok is false for anything that is not a winio pipe connection and on any
// failure, so a TCP connection can never yield a peer. Callers treat false as
// "no credential".
func PeerCred(conn net.Conn) (Peer, bool) {
	// TWO assertions, and both are load-bearing.
	//
	// winio.PipeConn (net.Conn + Disconnect + Flush) proves this is a winio
	// pipe and nothing else, which is what keeps a TCP caller out. It does
	// NOT carry Fd(): the accepted conns are the unexported *win32Pipe, and
	// Fd() is promoted from the embedded, also unexported *win32File. So the
	// handle needs its own assertion — but asserting ONLY on Fd() would admit
	// any future net.Conn that happens to expose one.
	pc, isPipe := conn.(winio.PipeConn)
	if !isPipe {
		return Peer{}, false
	}
	fder, hasFd := pc.(interface{ Fd() uintptr })
	if !hasFd {
		return Peer{}, false
	}
	handle := windows.Handle(fder.Fd())

	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(handle, &pid); err != nil {
		log.Debug().Err(err).Msg("auth: GetNamedPipeClientProcessId failed; no verified peer for this connection")
		return Peer{}, false
	}
	sid, err := pipeClientSID(handle)
	if err != nil {
		// Debug, not Warn: a caller with no identity is a legitimate outcome
		// (see the anonymous case below) and the request still gets the
		// unauthenticated treatment. But it is silent otherwise, and "the
		// bridge quietly became anonymous" is a miserable thing to diagnose
		// from nothing at all.
		log.Debug().Err(err).Uint32("client_pid", pid).
			Msg("auth: could not read the pipe client's SID; no verified peer for this connection")
		return Peer{}, false
	}
	return Peer{ID: localID(sid), Via: LocalVia, PID: int(pid)}, true
}

// pipeClientSID impersonates the pipe client just long enough to read its
// token user, on a thread that is LOCKED for the duration and RETIRED if the
// revert fails.
//
// Impersonation is a property of the OS THREAD, not of the goroutine: while
// it is in effect, every syscall that thread makes runs as the client. Go
// moves goroutines between threads at any preemption point, so the sequence
// runs on a dedicated goroutine under runtime.LockOSThread with no other work
// on it.
//
// If RevertToSelf fails, the thread is still impersonating and must never go
// back to the scheduler — some later, unrelated goroutine would run as the
// client. Leaving the thread LOCKED when the goroutine exits makes the Go
// runtime terminate it, which is the only safe disposal. That is why the
// unlock is conditional and not a defer.
func pipeClientSID(pipe windows.Handle) (string, error) {
	type result struct {
		sid string
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		sid, err, clean := readClientSID(pipe)
		if clean {
			runtime.UnlockOSThread()
		}
		done <- result{sid, err}
	}()
	r := <-done
	return r.sid, r.err
}

// readClientSID does the impersonation proper. clean reports whether the
// thread it ran on is safe to reuse: either it never impersonated, or it
// reverted successfully.
func readClientSID(pipe windows.Handle) (sid string, err error, clean bool) {
	if ferr := procImpersonateNamedPipeClient.Find(); ferr != nil {
		// LazyProc.Call would PANIC on a missing proc. A server must not die
		// because one connection could not be identified.
		return "", fmt.Errorf("ImpersonateNamedPipeClient unavailable: %w", ferr), true
	}
	r, _, errno := procImpersonateNamedPipeClient.Call(uintptr(pipe))
	if r == 0 {
		// Never impersonated, so the thread is untouched and reusable.
		return "", fmt.Errorf("ImpersonateNamedPipeClient: %w", errno), true
	}
	// From here on the thread IS impersonating. `clean` is decided by the
	// deferred RevertToSelf below and by nothing else, including on a panic,
	// so every return past this point leaves it alone (naked returns).
	defer func() {
		if rerr := windows.RevertToSelf(); rerr != nil {
			clean = false
			if err == nil {
				err = fmt.Errorf("RevertToSelf: %w", rerr)
			}
			return
		}
		clean = true
	}()

	var token windows.Token
	// openAsSelf = true: open the token using THIS process's security
	// context rather than the client's, which is what lets a server read the
	// token of a client less privileged than itself.
	if terr := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token); terr != nil {
		// ERROR_CANT_OPEN_ANONYMOUS (1347) arrives here when the client
		// dialled at SECURITY_ANONYMOUS — which is what go-winio's
		// DialPipeContext does by default. There is no identity to open at
		// that level, so a client that wants to be recognised must dial at
		// PipeImpLevelIdentification; auth.DialLocal does.
		err = fmt.Errorf("OpenThreadToken: %w", terr)
		return
	}
	defer token.Close()

	u, uerr := token.GetTokenUser()
	if uerr != nil {
		err = fmt.Errorf("GetTokenUser for the pipe client: %w", uerr)
		return
	}
	sid = u.User.Sid.String()
	return
}
