//go:build !linux && !darwin && !windows

package crashdump

import (
	"errors"
	"os"
)

// dupToStderr is unsupported on platforms without a dup2/dup3 syscall binding;
// RedirectStderr surfaces this so the caller can warn and continue.
func dupToStderr(*os.File) error {
	return errors.New("stderr redirect unsupported on this platform")
}
