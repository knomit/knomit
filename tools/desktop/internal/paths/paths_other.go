//go:build !darwin && !linux && !windows

package paths

import "knomit/internal/config"

// stateDir has no answer off the three desktop platforms; config says so.
func stateDir() (string, error) { return config.StateDir() }

func logsDir() (string, error) { return stateDir() }
