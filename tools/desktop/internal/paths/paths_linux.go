//go:build linux

package paths

import "knomit/internal/apppaths"

func stateDir() (string, error) { return apppaths.StateDir() }

func logsDir() (string, error) { return stateDir() }
