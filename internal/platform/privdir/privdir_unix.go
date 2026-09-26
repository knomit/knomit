//go:build !windows

package privdir

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/rs/zerolog/log"
)

// ensure creates path with mode 0700 when it is missing, and TIGHTENS an
// existing directory that is readable or writable by group or other to 0700
// when this user owns it, logging that once, on the boot that changes it.
//
// Tightening rather than only warning is deliberate. The data root is the
// application's own directory and the contract is 0700; in practice existing
// roots are 0755, because the earliest writers created them before anything
// asked for 0700 (the serve crash marker, the desktop's bin directory, the
// model download, a Docker image's COPY), and a warning on every boot of
// every install would be noise nobody acts on. The same rule holds on
// Windows: a root that merely inherited its access is fixed.
//
// A wide root owned by SOMEONE ELSE is not ours to change: it is reported,
// never chmodded. So is one whose owner cannot be read, and so is a root that
// is a SYMLINK, since chmod would follow it to a directory that may not be
// ours at all. Ownership is the EFFECTIVE uid: `sudo knomit` pointed at a
// user's home takes the warn path rather than chmodding their directory.
// Only the root itself is ever chmodded, never recursively. The info line
// names the old mode, so a root that was shared on purpose can be restored.
//
// Until this existed, the "0700 data root" that internal/config relies on
// was not enforced anywhere: ensureKeyPair's MkdirAll(dir, 0700)
// (internal/app/identity.go) only appeared to set it, since by then the
// root already existed at 0755.
func ensure(path string) error {
	lfi, err := os.Lstat(path)
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
	fi, err := os.Stat(path) // through a symlink: the directory actually used
	if err != nil {
		return err
	}
	perm := fi.Mode().Perm()
	if perm&0o077 == 0 {
		return nil
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		log.Warn().Str("path", path).Str("mode", perm.String()).
			Msg("data directory is a symlink to a directory readable or writable by other users; it is not tightened through the link, and anything stored in it is exposed to them")
		return nil
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		log.Warn().Str("path", path).Str("mode", perm.String()).
			Msg("data directory is readable or writable by other users and is not owned by this user; it is left as it is, but anything stored in it is exposed to them")
		return nil
	}
	if err := os.Chmod(path, 0o700); err != nil {
		log.Warn().Err(err).Str("path", path).Str("mode", perm.String()).
			Msg("data directory is readable or writable by other users and could not be tightened to 0700")
		return nil
	}
	log.Info().Str("path", path).Msgf("tightened data root to 0700 (was %o)", perm)
	return nil
}
