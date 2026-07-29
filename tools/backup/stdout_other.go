//go:build !unix

package main

import "os"

// claimProtocolStream is the fallback where no dup2 exists: the protocol keeps
// file descriptor 1, and the guard is the os.Stdout reassignment in main alone.
//
// That covers Go code resolving os.Stdout at call time, which is everything
// this agent's dependency tree does today. What it cannot cover is a
// package-level variable that captured the real fd 1 before main ran — see
// claimProtocolStream in stdout_unix.go for why that matters. knomit's server
// and its agent are deployed on unix, so this path is a compile-time
// convenience rather than a supported configuration.
func claimProtocolStream() (*os.File, error) { return os.Stdout, nil }
