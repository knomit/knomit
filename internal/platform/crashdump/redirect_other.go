//go:build !linux && !darwin

package crashdump

import "errors"

// dupToStderr is unsupported on platforms without a dup2/dup3 syscall binding;
// RedirectStderr surfaces this so the caller can warn and continue.
func dupToStderr(int) error {
	return errors.New("stderr redirect unsupported on this platform")
}
