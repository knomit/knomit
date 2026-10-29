//go:build !linux && !windows && !darwin

package paths

func downloadsDir() string { return homeDownloads() }
