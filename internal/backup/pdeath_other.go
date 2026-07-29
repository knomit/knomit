//go:build !linux

package backup

import "os/exec"

// setParentDeathSignal is a no-op everywhere but Linux: no other platform
// knomit builds for has an equivalent.
//
// Nothing is lost on those platforms, because the mechanism that actually
// guarantees the agent does not outlive knomit is portable — knomit's death
// closes the write end of the agent's stdin, the agent reads EOF and shuts
// down, and that holds under SIGKILL because the kernel closes the pipe when
// the process dies. See conn.terminate and backupagent.Agent.Serve.
func setParentDeathSignal(cmd *exec.Cmd) {}
