//go:build !darwin

package autostart

import (
	"fmt"
	"runtime"
)

type stub struct{}

func newToggler() Toggler { return stub{} }

func (stub) Enabled() (bool, error) { return false, nil }
func (stub) Enable() error          { return fmt.Errorf("autostart: unsupported platform %s", runtime.GOOS) }
func (stub) Disable() error         { return nil }
