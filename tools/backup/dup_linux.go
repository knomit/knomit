//go:build linux

package main

import "syscall"

// dupOnto points newfd at whatever oldfd refers to.
//
// Linux gets Dup3 rather than Dup2: syscall.Dup2 is absent on arm64 (the
// kernel dropped the dup2 syscall there), and knomit's own container images are
// built for both amd64 and arm64. Dup3 with flags 0 is exactly Dup2.
func dupOnto(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }
