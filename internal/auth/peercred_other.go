//go:build !linux && !darwin

package auth

func peerCredFD(int) (uid, pid int, ok bool) { return 0, 0, false }
