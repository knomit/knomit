package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"knomit/internal/client/sessions"
	"knomit/internal/platform/version"
)

// buildIdentity describes THIS bridge process. Every process is its own
// instance (master, worker and reviewer on one project are three rows): the
// hash covers pid and start time as well as host, user, cwd and parent app,
// so a pid reused after a reboot does not collapse onto an older run.
// Nothing is persisted. All of it is self-declared — correlation, not
// identity — see kb/decisions/mcp/client-sessions/identity-model.
func buildIdentity(branch string, startedAt time.Time) sessions.BridgeInfo {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	pid, ppid := os.Getpid(), os.Getppid()
	parent := parentAppName(ppid)
	return sessions.BridgeInfo{
		InstanceID: sessions.DeriveInstanceID(host, username, cwd, parent,
			strconv.Itoa(pid), strconv.FormatInt(startedAt.UnixNano(), 10)),
		Transport: "stdio",
		PID:       pid,
		ParentPID: ppid,
		ParentApp: parent,
		Host:      host,
		User:      username,
		Cwd:       cwd,
		Branch:    branch,
		Version:   version.String(),
	}
}

// parentAppName returns the executable base name of pid, or "" on any
// failure — identity still works with the field empty, so this never
// returns an error. Linux reads /proc/<pid>/comm; other platforms shell out
// to ps.
func parentAppName(pid int) string {
	if pid <= 0 {
		return ""
	}
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(bytes.TrimSpace(out))))
}

// clientHeaders is the fixed header set every outgoing request carries, so a
// session that outlived a server restart is fully described on its first
// post-restart touch.
func clientHeaders(ident sessions.BridgeInfo) http.Header {
	h := http.Header{}
	h.Set("User-Agent", "knomit-bridge/"+version.String())
	h.Set(sessions.ClientHeader, ident.Encode())
	return h
}
