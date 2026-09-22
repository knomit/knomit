//go:build darwin

package auth

import "golang.org/x/sys/unix"

func peerCredFD(fd int) (uid, pid int, ok bool) {
	x, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, 0, false
	}
	// LOCAL_PEERPID is a separate getsockopt (both constants exist in
	// golang.org/x/sys v0.46.0). A failure here leaves pid 0 and is not
	// fatal: the uid is the credential, the pid is for the session trail.
	pid, perr := unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if perr != nil {
		pid = 0
	}
	return int(x.Uid), pid, true
}
