//go:build !linux && !darwin && !windows

package auth

func peerCredFD(int) (uid, pid int, ok bool) { return 0, 0, false }
