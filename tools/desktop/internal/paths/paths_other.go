//go:build !darwin && !linux && !windows

package paths

import "knomit/internal/apppaths"

// stateDir has no answer off the three desktop platforms; apppaths says so.
func stateDir() (string, error) { return apppaths.StateDir() }

func logsDir() (string, error) { return stateDir() }
