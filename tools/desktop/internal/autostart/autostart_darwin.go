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

const plistLabel = "com.knomit.desktop"

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
	// resolveErr is the failure from newToggler, carried rather than dropped.
	// Toggler is constructed by New() Toggler, which has nowhere to return an
	// error, so every method reports it instead.
	resolveErr error
}

// newToggler resolves the two paths a launch agent needs: the plist to write,
// and the program for it to run.
//
// NEITHER failure is swallowed, and each is silent in its own way. An empty
// home makes filepath.Join("", "Library", …) a RELATIVE path, so Enable() would
// write the agent into the process's working directory and report success. An
// empty os.Executable() renders a plist whose ProgramArguments is an empty
// string — a well-formed file launchd accepts and can never start. Both leave a
// checkbox that is on and does nothing, which is the failure this toggle is
// least able to show the user.
func newToggler() Toggler {
	self, err := os.Executable()
	if err != nil {
		return &darwin{resolveErr: fmt.Errorf("locate this executable for the launch agent: %w", err)}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return &darwin{resolveErr: fmt.Errorf("locate home directory for the launch agent: %w", err)}
	}
	return &darwin{
		binaryPath: self,
		plistPath:  filepath.Join(home, "Library", "LaunchAgents", plistLabel+".plist"),
		loader:     launchctl{},
	}
}

func (d *darwin) Enabled() (bool, error) {
	if d.resolveErr != nil {
		return false, d.resolveErr
	}
	_, err := os.Stat(d.plistPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *darwin) Enable() error {
	if d.resolveErr != nil {
		return d.resolveErr
	}
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
	if d.resolveErr != nil {
		return d.resolveErr
	}
	_ = d.loader.unload(d.plistPath)
	if err := os.Remove(d.plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
