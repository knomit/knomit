package knomitapi

import (
	"context"
	"fmt"
	"os"

	"knomit/internal/pki"
)

// lockFile takes the cross-process credentials lock, waiting for a holder —
// a second kb waits for the first one's refresh to finish, then finds the
// new pair on disk — but only as long as ctx allows (3a review N5): a holder
// that is stopped or stuck must not stall this kb past its own deadline. The
// flock/LockFileEx code is pki's (pki.LockFile), shared with identity
// install's lock rather than copied.
func lockFile(ctx context.Context, f *os.File) error {
	err := pki.LockFile(ctx, f)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("waiting for the credentials lock (another kb is refreshing): %w", err)
	}
	return err
}

func unlockFile(f *os.File) error { return pki.UnlockFile(f) }
