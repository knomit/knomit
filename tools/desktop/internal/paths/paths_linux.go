//go:build linux

package paths

import "knomit/internal/config"

func stateDir() (string, error) { return config.StateDir() }

func logsDir() (string, error) { return stateDir() }
