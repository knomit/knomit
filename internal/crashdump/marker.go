package crashdump

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Marker is a liveness file that distinguishes a clean shutdown from a crash.
// Begin writes the file at startup; End removes it on clean shutdown. If Begin
// finds the file already present, the previous run exited without calling End —
// i.e. it crashed — and Begin reports that, making a restart-loop visible
// without any external supervisor.
type Marker struct {
	path string
}

// NewMarker returns a Marker backed by the file at path.
func NewMarker(path string) *Marker {
	return &Marker{path: path}
}

// Begin records that the process is running, starting at now. It returns
// whether a prior run crashed (the marker was already present) along with that
// run's recorded start time.
func (m *Marker) Begin(now time.Time) (priorCrash bool, priorStart time.Time, err error) {
	if data, readErr := os.ReadFile(m.path); readErr == nil {
		priorCrash = true
		if t, perr := time.Parse(time.RFC3339Nano, string(data)); perr == nil {
			priorStart = t
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return false, time.Time{}, fmt.Errorf("read crash marker: %w", readErr)
	}

	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return priorCrash, priorStart, fmt.Errorf("create marker dir: %w", err)
	}
	if err := os.WriteFile(m.path, []byte(now.UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		return priorCrash, priorStart, fmt.Errorf("write crash marker: %w", err)
	}
	return priorCrash, priorStart, nil
}

// End removes the marker, recording a clean shutdown. It is a no-op if the
// marker is already gone.
func (m *Marker) End() error {
	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove crash marker: %w", err)
	}
	return nil
}
