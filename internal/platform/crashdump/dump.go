package crashdump

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DumpGoroutines writes the stack traces of every goroutine to a timestamped
// file under dir and returns the path. It does NOT exit the process — it is the
// on-demand counterpart to a crash dump, used to inspect a stuck-but-live
// server (wired to SIGUSR1). The directory is created on demand.
func DumpGoroutines(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create dump dir: %w", err)
	}
	name := fmt.Sprintf("goroutines-%s.txt", time.Now().UTC().Format("20060102T150405.000Z"))
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create dump file: %w", err)
	}
	defer f.Close()

	if err := writeAllGoroutines(f); err != nil {
		return "", fmt.Errorf("write goroutine dump: %w", err)
	}
	return path, nil
}
