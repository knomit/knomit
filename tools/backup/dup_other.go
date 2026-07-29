//go:build unix && !linux

package main

import "syscall"

// dupOnto points newfd at whatever oldfd refers to. Everything unix that is
// not Linux (darwin, the BSDs) has dup2.
func dupOnto(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }
