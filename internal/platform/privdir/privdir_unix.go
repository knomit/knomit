//go:build !windows

package privdir

import (
	"errors"
	"io/fs"
	"os"

	"github.com/rs/zerolog/log"
)

// ensure creates path with mode 0700 when it is missing. An existing
// directory is NEVER chmodded: its mode is what the user set, and a boot that
// silently narrows it would break whatever they widened it for (a group that
// shares the root, say). A directory readable or writable by group or other
// is reported instead.
//
// This makes explicit the 0700 data root that internal/config (config.go, and
// paths_unix.go's socket path) already relies on. Until now the root was 0700
// only as a side effect of ensureKeyPair's MkdirAll of the key's directory
// (internal/app/identity.go), which a [remote].ssh_key outside the root
// bypassed entirely.
func ensure(path string) error {
	fi, err := os.Stat(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if !existed {
		return nil
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		log.Warn().Str("path", path).Str("mode", perm.String()).
			Msg("data directory is readable or writable by other users; it is left as it is, but anything stored in it is exposed to them (chmod 700 to make it private)")
	}
	return nil
}
