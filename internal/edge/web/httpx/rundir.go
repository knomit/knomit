package httpx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/foundation/pki"
)

// RUN DIRECTORIES. A package that keeps ephemeral per-process folders (the
// origin sessions, the chat transcripts) keeps them in a run directory of its
// own under a base directory:
//
//	<base>/run-<id>.lock   the run's liveness lock, held for the owner's life
//	<base>/run-<id>/       the run directory
//
// Two processes may share one home, and so one base (a `knomit serve` beside
// the desktop's), so liveness is the OS file lock (pki.LockFile: flock, or
// LockFileEx on Windows) on the lock file, never a pid: the kernel drops it
// when its holder dies. A sweep removes the runs of dead owners, those whose
// lock it can take, and never one whose lock is held. Every removal checks
// the path is the layout's (IsChild).
const (
	RunPrefix     = "run-"
	RunLockSuffix = ".lock"
)

// runLockWait bounds how long ClaimRun waits for its own new lock file, which
// only a sweeper probing it at that instant can hold (for one RemoveAll of a
// directory that does not exist yet). A latency bound, not a measured property.
const runLockWait = 5 * time.Second

// runProbeWait is how long a sweep tries a run's lock before deciding its
// owner is alive. Any live owner holds its lock for its whole life, so one
// try decides; the wait only covers pki.LockFile's polling.
const runProbeWait = 50 * time.Millisecond

// RunHooks are test hooks at the steps of the claim/reclaim race. Production
// passes nil. They are a parameter, not package variables, so the background
// sweeps other tests start can never call them.
type RunHooks struct {
	ReclaimOpened   func(lockPath string) // the sweep has opened the lock file, not yet probed it
	ReclaimLocked   func(lockPath string) // the sweep holds the lock
	ReclaimUnlocked func(lockPath string) // the sweep has just released it
	ClaimCreated    func(lockPath string) // ClaimRun has created the file, not yet locked it
}

func fire(h *RunHooks, f func(*RunHooks) func(string), lockPath string) {
	if h == nil {
		return
	}
	if fn := f(h); fn != nil {
		fn(lockPath)
	}
}

func hookReclaimOpened(h *RunHooks) func(string)   { return h.ReclaimOpened }
func hookReclaimLocked(h *RunHooks) func(string)   { return h.ReclaimLocked }
func hookReclaimUnlocked(h *RunHooks) func(string) { return h.ReclaimUnlocked }
func hookClaimCreated(h *RunHooks) func(string)    { return h.ClaimCreated }

// AcquireRunDir makes a new run directory under base (made 0700 when absent)
// and returns it with the release func that removes it and drops its lock.
// release is safe to call more than once. A cancelled ctx refuses the claim.
func AcquireRunDir(ctx context.Context, base string) (dir string, release func(), err error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, fmt.Errorf("create run base dir: %w", err)
	}
	lock, dir, err := ClaimRun(base, nil)
	if err != nil {
		return "", nil, err
	}
	var once sync.Once
	return dir, func() { once.Do(func() { ReleaseRun(base, lock, dir) }) }, nil
}

// ReleaseRun removes a claimed run's directory, then drops its lock and
// removes the lock file. The caller closes whatever it holds in the run first
// (Windows cannot remove a folder with an open file in it).
func ReleaseRun(base string, lock *os.File, dir string) {
	RemoveChild(base, dir, RunPrefix, "")
	lockPath := lock.Name()
	_ = pki.UnlockFile(lock)
	_ = lock.Close()
	RemoveChild(base, lockPath, RunPrefix, RunLockSuffix)
}

// SweepDeadRuns removes every run in base whose owner is dead (whose lock file
// it can lock) and returns how many it removed. A run whose lock is held is
// left alone, whoever holds it. A missing base is not an error.
func SweepDeadRuns(base string) (int, error) {
	return SweepDeadRunsHooked(base, nil)
}

// SweepDeadRunsHooked is SweepDeadRuns with test hooks; h is nil outside tests.
func SweepDeadRunsHooked(base string, h *RunHooks) (int, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list previous runs in %s: %w", base, err)
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, RunPrefix) || !strings.HasSuffix(name, RunLockSuffix) {
			continue
		}
		if reclaimRun(base, filepath.Join(base, name), h) {
			n++
		}
	}
	return n, nil
}

// reclaimRun removes the run whose lock file is lockPath if, and only if, it
// can take that lock. It removes the run directory, and unlinks the lock file,
// while holding the lock (on Windows the unlink waits for the close; see
// below). A claimer racing this on a brand-new lock file then finds its lock
// on an unlinked file and starts over (ClaimRun). It reports whether the run
// was removed.
func reclaimRun(base, lockPath string, h *RunHooks) bool {
	if !IsChild(base, lockPath, RunPrefix, RunLockSuffix) {
		return false
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	fire(h, hookReclaimOpened, lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), runProbeWait)
	err = pki.LockFile(ctx, f)
	cancel()
	if err != nil {
		_ = f.Close() // held: the owner is alive
		return false
	}
	fire(h, hookReclaimLocked, lockPath)
	dir := strings.TrimSuffix(lockPath, RunLockSuffix)
	removed := RemoveChild(base, dir, RunPrefix, "")
	// Unlink the lock file WHILE HOLDING its lock. Unlinking after the unlock
	// would race a claimer that created this file a moment ago (ClaimRun): it
	// can take the lock in the gap, pass its sameFileAt check and make its run
	// directory, and the late unlink would then strip a live run of its lock
	// file. Unlinked while held, the claimer's lock lands on an unlinked file,
	// its sameFileAt check fails, and it starts over.
	//
	// Windows refuses to remove a file that has an open handle (Go opens files
	// without FILE_SHARE_DELETE), including this one, so there the unlink
	// waits until after the close. That is safe on Windows: a claimer holds
	// its handle open from CreateTemp for the run's whole life, so the
	// removal fails while any claimer has the file, and succeeds only once
	// nobody does.
	//
	// A run whose directory could not be removed keeps its lock file, so the
	// next start retries it.
	unlinked := false
	if removed {
		unlinked = os.Remove(lockPath) == nil // IsChild-checked above
	}
	_ = pki.UnlockFile(f)
	fire(h, hookReclaimUnlocked, lockPath)
	_ = f.Close()
	if removed && !unlinked {
		_ = os.Remove(lockPath) // Windows; see above. Fails while a claimer has it open.
	}
	if removed {
		log.Info().Str("run", filepath.Base(dir)).Msg("run dirs: removed the folders of a run whose process has exited")
	}
	return removed
}

// ClaimRun makes a new run under base: its lock file first, locked, and only
// then its directory, so a sweeper never sees a live run without a held lock.
// It returns the held lock file and the run directory. h is nil outside tests.
func ClaimRun(base string, h *RunHooks) (*os.File, string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.CreateTemp(base, RunPrefix+"*"+RunLockSuffix)
		if err != nil {
			return nil, "", fmt.Errorf("create run lock: %w", err)
		}
		fire(h, hookClaimCreated, f.Name())
		ctx, cancel := context.WithTimeout(context.Background(), runLockWait)
		err = pki.LockFile(ctx, f)
		cancel()
		if err != nil {
			_ = f.Close()
			return nil, "", fmt.Errorf("lock run %s: %w", f.Name(), err)
		}
		// A sweeper that probed the new file before it was locked took it for
		// a dead run's and removed it: the lock is then on an unlinked file.
		if fi, serr := f.Stat(); serr != nil || !sameFileAt(fi, f.Name()) {
			_ = pki.UnlockFile(f)
			_ = f.Close()
			continue
		}
		dir := strings.TrimSuffix(f.Name(), RunLockSuffix)
		if err := os.Mkdir(dir, 0o700); err != nil {
			_ = pki.UnlockFile(f)
			_ = f.Close()
			RemoveChild(base, f.Name(), RunPrefix, RunLockSuffix)
			return nil, "", fmt.Errorf("create run dir: %w", err)
		}
		return f, dir, nil
	}
	return nil, "", errors.New("create run: lost the new lock file to a concurrent sweep three times")
}

// sameFileAt reports whether path still names the file fi describes.
func sameFileAt(fi os.FileInfo, path string) bool {
	at, err := os.Stat(path)
	return err == nil && os.SameFile(fi, at)
}

// IsChild reports whether p is a direct child of parent whose name starts
// with prefix and ends with suffix: the only shape RemoveChild removes.
func IsChild(parent, p, prefix, suffix string) bool {
	if parent == "" || p == "" {
		return false
	}
	p = filepath.Clean(p)
	name := filepath.Base(p)
	return filepath.Dir(p) == filepath.Clean(parent) &&
		strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) &&
		len(name) > len(prefix)+len(suffix)
}

// RemoveChild removes p, and everything in it, if IsChild(parent, p, prefix,
// suffix); it refuses, and logs, anything else. It reports whether p is gone.
func RemoveChild(parent, p, prefix, suffix string) bool {
	if !IsChild(parent, p, prefix, suffix) {
		log.Warn().Str("path", p).Str("parent", parent).Msg("run dirs: refusing to remove a path outside the run layout")
		return false
	}
	if err := os.RemoveAll(p); err != nil {
		log.Warn().Err(err).Str("path", p).Msg("run dirs: removal failed")
		return false
	}
	return true
}
