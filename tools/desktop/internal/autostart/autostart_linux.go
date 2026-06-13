//go:build linux

package autostart

import (
	"os"
	"os/exec"
	"path/filepath"
)

const serviceName = "knomit-desktop.service"

type linuxToggler struct{}

func newToggler() Toggler { return linuxToggler{} }

func serviceFilePath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "systemd", "user", serviceName)
}

func (linuxToggler) Enabled() (bool, error) {
	err := exec.Command("systemctl", "--user", "is-enabled", "--quiet", serviceName).Run()
	return err == nil, nil
}

func (linuxToggler) Enable() error {
	return exec.Command("systemctl", "--user", "enable", serviceName).Run()
}

func (linuxToggler) Disable() error {
	return exec.Command("systemctl", "--user", "disable", serviceName).Run()
}
