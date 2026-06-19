// Package lockfile reads and writes the knomit server discovery file.
package lockfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrCorrupt wraps a lockfile that exists but cannot be parsed. Callers
// distinguish it from a genuine I/O error (e.g. permission denied): a corrupt
// lockfile records no usable instance and is safe to overwrite, whereas an I/O
// error is a real problem the caller should surface.
var ErrCorrupt = errors.New("corrupt lockfile")

type Info struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Version string `json:"version"`
}

// Write serializes info to path atomically (write-then-rename) with 0600 perms.
// Creates parent directories as needed.
func Write(path string, info Info) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lockfile: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "server-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create tempfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename tempfile: %w", err)
	}
	return nil
}

// Read returns the Info recorded in the lockfile at path.
// If the file does not exist, returns an error wrapping os.ErrNotExist.
func Read(path string) (Info, error) {
	var info Info
	data, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, fmt.Errorf("%w: parse %s: %v", ErrCorrupt, path, err)
	}
	return info, nil
}

// Remove deletes the lockfile at path. Missing file is not an error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
