// Package homelock guards KNOMIT_HOME against two processes claiming it at
// once.
//
// Two commands take it, from opposite sides of the same guarantee, and both
// directions have to hold or neither does: `knomit serve`, which OPENS a home
// and refuses to start when the claim is held, and `knomit restore`, which
// REWRITES one and refuses for the same reason.
//
// The knomit-desktop app deliberately does NOT take it. It cannot create the
// hazard below, because backup is compiled out of the desktop build
// (config.applyBackupBuildPolicy) rather than merely switched off — see the
// project-owner ruling recorded there.
//
// # What it prevents
//
// Two servers on one home each replicate to the SAME object-store prefix, so
// two litestream agents write one LTX chain — the condition knomit deliberately
// never auto-repairs, because repairing it means discarding backup history.
// Nothing else catches it in time: a port collision is detected asynchronously,
// after every database has already been tracked, and a second server on a
// different port never collides at all.
//
// `knomit restore` is the only path permitted to overwrite a live database, and
// running it against a RUNNING server replaces a file two processes hold open,
// clears the sidecars the running one is writing, and deletes the litestream
// state directory the backup agent is actively using. The backup agent's own
// "am I replicating this path" check cannot see that: restore spawns a FRESH
// agent whose tracked set is empty, so that check guards a caller that does not
// exist.
//
// # Why an advisory file lock rather than a PID file
//
// A PID file has to answer "is that process still alive", and every answer is
// wrong somewhere: the pid may have been reused, the check needs signal
// permission it may not have, and a crash leaves a file that either blocks
// recovery forever or is ignored and protects nothing. Recovery is EXACTLY when
// this command is needed, so a stale lock that blocks it would be worse than no
// lock at all.
//
// An advisory lock has none of that: the kernel releases it when the holding
// process dies, for any reason including SIGKILL, because the descriptor is
// closed as the process is reaped. A crashed server therefore leaves the lock
// FILE behind but not the LOCK, and the next Acquire succeeds. Pinned by
// TestAcquireSucceedsAfterTheHolderIsKilled, which kills a real child process.
//
// The pid written into the file is for the error MESSAGE only. Nothing branches
// on it, so it cannot become a second, worse liveness check.
//
// # Platform coverage
//
// Real locking is provided wherever Go's `unix` build tag applies: linux,
// darwin, the BSDs, solaris and aix — every platform knomit is deployed or
// developed on. On windows, plan9 and js, flock_other.go supplies a no-op that
// always reports the lock as taken.
//
// The consequence on those three platforms is concrete and must not be glossed:
// `knomit serve` cannot detect a second server, and `knomit restore` cannot
// detect a running one, so restore's refusal simply never fires. A no-op is
// still the right default there — failing every Acquire would make knomit
// unstartable on a platform where nothing is actually wrong — but callers that
// advertise the protection in help text owe the reader that caveat, and
// cmd/restore.go carries it.
package homelock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LockFile is the name of the lock file inside KNOMIT_HOME.
const LockFile = "knomit.lock"

// ErrHeld means another live process holds the lock on that KNOMIT_HOME.
var ErrHeld = errors.New("another knomit process is using this KNOMIT_HOME")

// Lock is a held claim on one KNOMIT_HOME. Release it when done; letting the
// process exit releases it too.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes an exclusive, non-blocking claim on home.
//
// It returns an error wrapping ErrHeld when a live process already holds it,
// with that process's pid in the message. It never waits: a caller that wanted
// to queue behind a running server would be a caller that should not run at
// all.
func Acquire(home string) (*Lock, error) {
	if home == "" {
		return nil, fmt.Errorf("homelock.Acquire: home is empty")
	}
	// The directory is NOT created here. Acquiring a claim is not a reason to
	// bring the thing being claimed into existence, and doing so left `knomit
	// restore` scattering empty directories at typo'd KNOMIT_HOME paths before
	// failing for an unrelated reason. The owner of the home creates it (see
	// cmd/serve.go); everyone else gets told it is not there.
	if fi, err := os.Stat(home); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("homelock.Acquire: %s does not exist", home)
		}
		return nil, fmt.Errorf("homelock.Acquire: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("homelock.Acquire: %s is not a directory", home)
	}
	path := filepath.Join(home, LockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("homelock.Acquire: open %s: %w", path, err)
	}

	locked, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("homelock.Acquire: lock %s: %w", path, err)
	}
	if !locked {
		holder := readPID(f)
		_ = f.Close()
		if holder > 0 {
			return nil, fmt.Errorf("%w: pid %d holds %s", ErrHeld, holder, path)
		}
		return nil, fmt.Errorf("%w: %s is locked", ErrHeld, path)
	}

	// Record the holder for whoever is refused next. Best effort: a failure here
	// costs a less specific error message later, and is not worth surrendering a
	// lock that was successfully taken.
	if err := f.Truncate(0); err == nil {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		}
	}
	return &Lock{f: f, path: path}, nil
}

// Release drops the lock. It is safe on a nil Lock, so callers can defer it
// unconditionally.
//
// The file is deliberately NOT removed. Unlinking it would let a process that
// has already opened the same path lock a file nobody can find, and two
// processes would then both believe they own the home — the exact outcome this
// package exists to prevent.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	// Closing the descriptor releases the lock on its own; unlocking first makes
	// the intent explicit and keeps the two steps independently correct.
	unlockErr := unlock(l.f)
	closeErr := l.f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// Path is the lock file's location, for error messages.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// readPID reads the holder's pid out of an already-open lock file. It returns 0
// when the file says nothing useful, which is normal — the file exists before
// anyone has written to it.
func readPID(f *os.File) int {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	// Bounded: the file holds a decimal pid and a newline, and an unbounded read
	// of a file another process is writing has no upside.
	buf := make([]byte, 32)
	n, err := f.Read(buf)
	if n == 0 || (err != nil && err != io.EOF) {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
