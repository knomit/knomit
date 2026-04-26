//go:build darwin

package autostart

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

const plistLabel = "com.knomit.tray"

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Binary}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`))

type launcher interface {
	load(plistPath string) error
	unload(plistPath string) error
}

type launchctl struct{}

func (launchctl) load(p string) error   { return exec.Command("launchctl", "load", "-w", p).Run() }
func (launchctl) unload(p string) error { return exec.Command("launchctl", "unload", "-w", p).Run() }

type darwin struct {
	binaryPath string
	plistPath  string
	loader     launcher
}

func newToggler() Toggler {
	self, _ := os.Executable()
	home, _ := os.UserHomeDir()
	return &darwin{
		binaryPath: self,
		plistPath:  filepath.Join(home, "Library", "LaunchAgents", plistLabel+".plist"),
		loader:     launchctl{},
	}
}

func (d *darwin) Enabled() (bool, error) {
	_, err := os.Stat(d.plistPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *darwin) Enable() error {
	if err := os.MkdirAll(filepath.Dir(d.plistPath), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}
	var buf bytes.Buffer
	if err := plistTemplate.Execute(&buf, map[string]string{
		"Label":  plistLabel,
		"Binary": d.binaryPath,
	}); err != nil {
		return fmt.Errorf("render plist: %w", err)
	}
	if err := os.WriteFile(d.plistPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if err := d.loader.load(d.plistPath); err != nil {
		// Non-fatal: the plist is already written and will be picked up at next login.
		return nil
	}
	return nil
}

func (d *darwin) Disable() error {
	_ = d.loader.unload(d.plistPath)
	if err := os.Remove(d.plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
