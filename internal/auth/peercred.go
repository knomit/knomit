package auth

import "net"

// PeerCred returns the kernel's view of who is on the other end of a Unix
// domain socket connection. This is the credential for the local bridge:
// no token is stored anywhere, the 0700 directory holding the socket is the
// gate, and the uid and pid come from the kernel rather than from a header
// the client wrote about itself (kb/decisions/mcp/client-sessions/identity-model
// records that today's client_sessions.pid is self-declared).
//
// ok is false for any connection that is not a *net.UnixConn and on any
// syscall failure; callers treat false as "no credential", never as uid 0.
func PeerCred(conn net.Conn) (uid, pid int, ok bool) {
	uc, isUnix := conn.(*net.UnixConn)
	if !isUnix {
		return 0, 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var (
		gotUID, gotPID int
		gotOK          bool
	)
	ctrlErr := raw.Control(func(fd uintptr) {
		gotUID, gotPID, gotOK = peerCredFD(int(fd))
	})
	if ctrlErr != nil || !gotOK {
		return 0, 0, false
	}
	return gotUID, gotPID, true
}
