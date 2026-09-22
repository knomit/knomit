//go:build linux

package auth

import "golang.org/x/sys/unix"

func peerCredFD(fd int) (uid, pid int, ok bool) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, 0, false
	}
	return int(cred.Uid), int(cred.Pid), true
}
