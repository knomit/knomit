package crashdump

import (
	"fmt"
	"os"
	"path/filepath"
)

// RedirectStderr points the process's stderr (fd 2) at an append-only file so
// that the Go runtime's fatal traceback — and a CGO/ONNX SIGSEGV traceback,
// which the runtime writes directly to fd 2, bypassing the logger — is
// persisted to disk. It returns the opened file, kept open for the process
// lifetime.
//
// Because anything writing to os.Stderr now also lands in this file, this is
// intended for non-interactive/daemon use. In containers, leave it off: the
// log driver already captures fd 2.
func RedirectStderr(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create crash-log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open crash log: %w", err)
	}
	if err := dupToStderr(int(f.Fd())); err != nil {
		f.Close()
		return nil, fmt.Errorf("redirect stderr: %w", err)
	}
	return f, nil
}
