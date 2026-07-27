package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

// releaseOf extracts the semver from a recorded tool_version, dropping the
// build SHA that version.String() appends ("0.5.0.78233d95" → "0.5.0").
//
// The release is the part that tracks whether the MAPPER may have changed; the
// SHA tracks only which binary ran. Comparing the whole string would make
// every rebuild look like a new mapper. An empty or SHA-less value (a bare
// `go build` reports just "dev") comes back unchanged.
func releaseOf(toolVersion string) string {
	// Major.Minor.Patch.<sha>: keep the first three fields, drop a fourth.
	// Anything with fewer fields is already SHA-less and passes through.
	fields := strings.SplitN(toolVersion, ".", 4)
	if len(fields) < 4 {
		return toolVersion
	}
	return strings.Join(fields[:3], ".")
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

// marshalConfig renders the config, header first. It deliberately does NOT
// write: the config is an OWNED path, so it must reach disk through the same
// reconcile-and-stage pass as every other bundle file. A convenience writer
// here would be the obvious thing to reach for and would silently bypass the
// prune, the changed-set accounting, and the staging — so there isn't one.
func marshalConfig(c Config) ([]byte, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte(configHeader), body...), nil
}
