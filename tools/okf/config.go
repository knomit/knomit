package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configFile is committed at the bundle root. It is conformance-safe: OKF's
// rules constrain only .md files, so a .yaml here is invisible to Validate.
const configFile = ".knomit-okf.yaml"

// configHeader marks the file as machine-maintained, so a publisher browsing
// their own repo knows not to hand-edit it.
const configHeader = "# maintained by knomit-okf — https://github.com/knomit/knomit\n"

// Config records what a sync needs to repeat itself. Source is written ONLY
// with --publish-source, so a private KB's address is never published by
// default; the git remote carries it locally either way.
type Config struct {
	Branch       string `yaml:"branch"`
	SyncedCommit string `yaml:"synced_commit"`
	ToolVersion  string `yaml:"tool_version,omitempty"`
	Source       string `yaml:"source,omitempty"`
}

// readConfig loads the config from dir. A missing file is not an error: it is
// how `sync -b <new-branch>` on a fresh output branch begins.
func readConfig(dir string) (Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", configFile, err)
	}
	return c, nil
}

// writeConfig writes the config to dir, header first.
func writeConfig(dir string, c Config) error {
	body, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, configFile), append([]byte(configHeader), body...), 0o644)
}

// marshalConfig renders the config exactly as writeConfig would, for callers
// that reconcile it through the bundle file set rather than writing it
// directly — the config is an OWNED path, so it must flow through the same
// write-and-prune pass as everything else.
func marshalConfig(c Config) ([]byte, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte(configHeader), body...), nil
}
