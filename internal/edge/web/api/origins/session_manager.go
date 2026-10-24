package origins

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

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
)

// authConfig holds authentication credentials for a remote connection attempt.
type authConfig struct {
	Method   string // "token", "basic", "ssh"
	Token    string
	User     string
	Password string
}

// sessionState represents the lifecycle stage of an originSession.
type sessionState string

const (
	stateCreated   sessionState = "created"
	stateTested    sessionState = "tested"
	statePreviewed sessionState = "previewed"
	stateApplied   sessionState = "applied"
	stateCommitted sessionState = "committed"
	stateError     sessionState = "error"
)

// sessionExpiry is the idle duration after which a session is eligible for cleanup.
const sessionExpiry = 10 * time.Minute

// cleanupInterval is how often the sweeper checks for expired sessions.
const cleanupInterval = 60 * time.Second

// The on-disk layout under a manager's base directory (see FOLDERS on
// sessionManager):
//
//	<base>/run-<id>.lock        the run's liveness lock, held for the manager's life
//	<base>/run-<id>/            the run directory
//	<base>/run-<id>/session-*   one folder per session
//
// Every removal checks the path against this layout (isChild), so nothing
// outside it can be deleted.
const (
	runPrefix        = "run-"
	runLockSuffix    = ".lock"
	sessionDirPrefix = "session-"
)

// privateRootPattern is the os.MkdirTemp pattern of the base a manager makes
// for itself when it is given none (see newSessionManager).
const privateRootPattern = "knomit-origin-sessions-"

// runLockWait bounds how long claimRun waits for its own new lock file, which
// only a sweeper probing it at that instant can hold (for one RemoveAll of a
// directory that does not exist yet). A latency bound, not a measured property.
const runLockWait = 5 * time.Second

// runProbeWait is how long the stale sweep tries a run's lock before deciding
// its owner is alive. Any live owner holds its lock for its whole life, so
// one try decides; the wait only covers pki.LockFile's polling.
const runProbeWait = 50 * time.Millisecond

// originSession holds ephemeral state for a single remote connection workflow.
// It is owned by the sessionManager and must not be shared across goroutines
// without holding mu.
//
// USE. RemoteStore and the folder TempDir are touched only by a handler that
// holds the session's use (sessionManager.Use: test, preview, apply, commit),
// or by finalize. Use is exclusive (busy has room for one), so two steps of
// one session never run at once. When the session is dropped (Delete, replace,
// expiry, lifetime end), retire finalizes it at once if no handler holds the
// use, and otherwise leaves that to the handler's release: so a store is never
// closed, and a folder never removed, under a handler using it.
type originSession struct {
	ID            string
	Repo          string
	URL           string
	Auth          authConfig
	TempDir       string
	State         sessionState
	CreatedAt     time.Time
	LastAccess    time.Time
	RemoteStore   *store.Service // cloned remote store, set by test handler
	AppliedBranch string         // agent branch written into the clone during apply; used by commit for rebuild
	// RemoteBranch is the upstream consensus branch chosen at /apply time
	// (explicit req.Branch, or the test-step default). The commit handler
	// must prefer this over TestResult.DefaultBranch so a user picking a
	// non-default branch (e.g. master when remote default is main) still
	// has the origin configured to track their choice.
	RemoteBranch  string
	TestResult    any // cached result from test step
	PreviewResult any // cached result from preview step
	ApplyResult   any // cached result from apply step
	mu            sync.Mutex

	runDir    string           // the run directory TempDir is in; immutable
	busy      chan struct{}    // the use's semaphore, room for one
	gone      chan struct{}    // closed by the first retire, so a waiting acquire gives up
	onFinal   func()           // called once, after finalize; immutable
	now       func() time.Time // the manager's clock, for release's touch; immutable
	inUse     bool             // a handler holds the use; guarded by mu
	dropped   bool             // the session has left the manager; guarded by mu
	finalized bool             // finalize has been claimed; guarded by mu
}

// acquire takes the session's use, waiting for another holder as long as
// ctx allows. It reports false, holding nothing, when ctx ends first or the
// session has been dropped (before or while it waits).
func (s *originSession) acquire(ctx context.Context) (release func(), ok bool) {
	select {
	case <-s.gone:
		return nil, false
	default:
	}
	select {
	case s.busy <- struct{}{}:
	case <-s.gone:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
	s.mu.Lock()
	if s.dropped {
		s.mu.Unlock()
		<-s.busy
		return nil, false
	}
	s.inUse = true
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(s.release) }, true
}

// release ends a use, finalizing the session if it was dropped meanwhile.
func (s *originSession) release() {
	s.mu.Lock()
	s.inUse = false
	// The idle clock restarts when the step ENDS: runCleanup skips a session
	// in use, so a step longer than sessionExpiry is not evicted under its
	// handler, and the 10 minutes count from here.
	s.LastAccess = s.now()
	final := s.dropped && !s.finalized
	if final {
		s.finalized = true
	}
	s.mu.Unlock()
	<-s.busy
	if final {
		s.finalize()
	}
}

// retire marks the session dropped and finalizes it now unless a handler
// holds its use (USE). Callers have already removed it from the manager's map
// and hold no manager lock: finalize does IO.
func (s *originSession) retire() {
	s.mu.Lock()
	if !s.dropped {
		close(s.gone)
	}
	s.dropped = true
	final := !s.inUse && !s.finalized
	if final {
		s.finalized = true
	}
	s.mu.Unlock()
	if final {
		s.finalize()
	}
}

// finalize closes the session's RemoteStore, THEN removes its folder: an
// open store holds files in it, which Windows cannot remove. It runs once,
// claimed through finalized.
func (s *originSession) finalize() {
	s.mu.Lock()
	st := s.RemoteStore
	s.RemoteStore = nil
	s.mu.Unlock()
	if st != nil {
		if err := st.Close(); err != nil {
			log.Warn().Err(err).Str("repo", s.Repo).Str("session_id", s.ID).Msg("origin session: closing the cloned store failed")
		}
	}
	removeChild(s.runDir, s.TempDir, sessionDirPrefix, "")
	if s.onFinal != nil {
		s.onFinal()
	}
}

// errSessionsClosed is Create's answer once the manager's lifetime has ended.
var errSessionsClosed = errors.New("origin sessions have shut down")

// sessionManager manages one ephemeral originSession per repo. Mount creates
// the one manager its routes share; nothing outside this package can reach it.
//
// ONE LOCK. mu guards the session map, and every change to it happens under
// mu together with the decision that caused it: Create replaces a repo's
// session and stores the new one in one critical section, Delete removes a
// session only if the one in the map is the exact one named, and the sweeper
// evicts what it found expired in the same section that found it. So no
// session can be stored behind a concurrent replace (and leak its folder),
// and an eviction can never take out a newer session that reused the repo
// key. No IO happens under mu: folders are made before it is taken, and a
// dropped session is retired (its store closed, its folder removed) after it
// is released. Lock order is mu, then a session's mu; runMu is never held
// with either.
//
// FOLDERS. The manager keeps its session folders in a run directory of its
// own under base, and holds an OS file lock (pki.LockFile: flock, or
// LockFileEx on Windows) on the run's lock file for its whole life. Two
// processes may share one home, and so one base (a `knomit serve` beside the
// desktop's), so liveness is that lock, never a pid: the kernel drops it when
// its holder dies. At construction the manager starts a background sweep that
// removes the runs of dead owners, i.e. those whose lock it can take, and
// never one whose lock is held. Every removal checks the path is the
// layout's (isChild). Given no base, the manager makes a private one under
// os.TempDir() on its first Create and removes it when its lifetime ends.
//
// LIFETIME. The manager lives as long as the ctx it was built with. When ctx
// ends it refuses further Creates and retires every session (a session in use
// finalizes when its handler releases it), then, once every session has
// finalized, removes its run directory and releases its lock. That runs on
// its own goroutine (context.AfterFunc) and nothing waits for it, so a process
// that exits right after can leave its run behind; the next manager over the
// same base reclaims it, since the lock died with the process. The idle-expiry
// sweeper runs only while the manager holds at least one session (Create
// starts it when none is running, and it exits on the first tick that finds
// the map empty), so a manager with no sessions owns no goroutine.
type sessionManager struct {
	now      func() time.Time // the clock expiry is judged by; time.Now outside tests
	interval time.Duration    // the sweeper's tick; cleanupInterval outside tests
	done     <-chan struct{}  // the manager's lifetime; nil never ends

	// staleDone is closed when the startup sweep of dead runs has finished
	// (at once when there is no base to sweep), for tests.
	staleDone chan struct{}

	mu       sync.Mutex
	sessions map[string]*originSession // by repo name
	sweeping bool                      // a sweeper goroutine is running
	closed   bool                      // the lifetime has ended
	live     sync.WaitGroup            // sessions not yet finalized; Add under mu while !closed

	runMu     sync.Mutex
	base      string   // see FOLDERS
	baseOwned bool     // base is the private one this manager made
	run       string   // the run directory, "" until the first Create
	runLock   *os.File // the run's lock file, locked while run != ""
	runClosed bool     // the lifetime has ended: no run may be made
}

// newSessionManager creates an empty sessionManager over base ("" for a
// private one, see FOLDERS) that lives as long as ctx. It starts the
// background sweep of dead runs in base, and no other goroutine: the first
// Create starts the sweeper (see LIFETIME).
func newSessionManager(ctx context.Context, base string) *sessionManager {
	sm := &sessionManager{
		now:       time.Now,
		interval:  cleanupInterval,
		done:      ctx.Done(),
		staleDone: make(chan struct{}),
		base:      base,
		sessions:  map[string]*originSession{},
	}
	if base != "" {
		go func() {
			defer close(sm.staleDone)
			reclaimDeadRuns(base)
		}()
	} else {
		close(sm.staleDone)
	}
	context.AfterFunc(ctx, sm.close)
	return sm
}

// reclaimDeadRuns removes every run in base whose owner is dead: whose lock
// file it can lock. A run whose lock is held is left alone, whoever holds it.
func reclaimDeadRuns(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn().Err(err).Str("dir", base).Msg("origin sessions: cannot list previous runs")
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, runPrefix) || !strings.HasSuffix(name, runLockSuffix) {
			continue
		}
		reclaimRun(base, filepath.Join(base, name), nil)
	}
}

// runHooks are test hooks at the steps of the claim/reclaim race
// (TestReclaimRun_NeverStripsANewRunOfItsLock). Production passes nil. They
// are a parameter, not package variables, so the background sweeps other
// tests start can never call them.
type runHooks struct {
	reclaimLocked   func(lockPath string) // reclaimRun holds the lock
	reclaimUnlocked func(lockPath string) // reclaimRun has just released it
	claimCreated    func(lockPath string) // claimRun has created the file, not yet locked it
}

// fire calls f(lockPath) when the hooks are set and f is one of them.
func (h *runHooks) fire(f func(*runHooks) func(string), lockPath string) {
	if h == nil {
		return
	}
	if fn := f(h); fn != nil {
		fn(lockPath)
	}
}

func hookReclaimLocked(h *runHooks) func(string)   { return h.reclaimLocked }
func hookReclaimUnlocked(h *runHooks) func(string) { return h.reclaimUnlocked }
func hookClaimCreated(h *runHooks) func(string)    { return h.claimCreated }

// reclaimRun removes the run whose lock file is lockPath if, and only if, it
// can take that lock. It removes the run directory, and unlinks the lock file,
// while holding the lock (on Windows the unlink waits for the close; see
// below). A claimer racing this on a brand-new lock file then finds its lock
// on an unlinked file and starts over (claimRun).
func reclaimRun(base, lockPath string, h *runHooks) {
	if !isChild(base, lockPath, runPrefix, runLockSuffix) {
		return
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runProbeWait)
	err = pki.LockFile(ctx, f)
	cancel()
	if err != nil {
		_ = f.Close() // held: the owner is alive
		return
	}
	h.fire(hookReclaimLocked, lockPath)
	dir := strings.TrimSuffix(lockPath, runLockSuffix)
	removed := removeChild(base, dir, runPrefix, "")
	// Unlink the lock file WHILE HOLDING its lock. Unlinking after the unlock
	// would race a claimer that created this file a moment ago (claimRun): it
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
		unlinked = os.Remove(lockPath) == nil // isChild-checked above
	}
	_ = pki.UnlockFile(f)
	h.fire(hookReclaimUnlocked, lockPath)
	_ = f.Close()
	if removed && !unlinked {
		_ = os.Remove(lockPath) // Windows; see above. Fails while a claimer has it open.
	}
	if removed {
		log.Info().Str("run", filepath.Base(dir)).Msg("origin sessions: removed the session folders of a run whose process has exited")
	}
}

// claimRun makes a new run under base: its lock file first, locked, and only
// then its directory, so a sweeper never sees a live run without a held lock.
// h is nil outside tests (runHooks).
func claimRun(base string, h *runHooks) (*os.File, string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.CreateTemp(base, runPrefix+"*"+runLockSuffix)
		if err != nil {
			return nil, "", fmt.Errorf("create origin sessions run lock: %w", err)
		}
		h.fire(hookClaimCreated, f.Name())
		ctx, cancel := context.WithTimeout(context.Background(), runLockWait)
		err = pki.LockFile(ctx, f)
		cancel()
		if err != nil {
			_ = f.Close()
			return nil, "", fmt.Errorf("lock origin sessions run %s: %w", f.Name(), err)
		}
		// A sweeper that probed the new file before it was locked took it for
		// a dead run's and removed it: the lock is then on an unlinked file.
		if fi, serr := f.Stat(); serr != nil || !sameFileAt(fi, f.Name()) {
			_ = pki.UnlockFile(f)
			_ = f.Close()
			continue
		}
		dir := strings.TrimSuffix(f.Name(), runLockSuffix)
		if err := os.Mkdir(dir, 0o700); err != nil {
			_ = pki.UnlockFile(f)
			_ = f.Close()
			removeChild(base, f.Name(), runPrefix, runLockSuffix)
			return nil, "", fmt.Errorf("create origin sessions run dir: %w", err)
		}
		return f, dir, nil
	}
	return nil, "", errors.New("create origin sessions run: lost the new lock file to a concurrent sweep three times")
}

// sameFileAt reports whether path still names the file fi describes.
func sameFileAt(fi os.FileInfo, path string) bool {
	at, err := os.Stat(path)
	return err == nil && os.SameFile(fi, at)
}

// runDir returns the manager's run directory, claiming it (and making a
// private base, when the manager has none) on first use. It does its IO under
// runMu only, never under mu.
func (sm *sessionManager) runDir() (string, error) {
	sm.runMu.Lock()
	defer sm.runMu.Unlock()
	if sm.runClosed {
		return "", errSessionsClosed
	}
	if sm.run != "" {
		return sm.run, nil
	}
	if sm.base == "" {
		b, err := os.MkdirTemp("", privateRootPattern)
		if err != nil {
			return "", fmt.Errorf("create origin sessions dir: %w", err)
		}
		sm.base, sm.baseOwned = b, true
	} else if err := os.MkdirAll(sm.base, 0o700); err != nil {
		return "", fmt.Errorf("create origin sessions dir: %w", err)
	}
	lock, dir, err := claimRun(sm.base, nil)
	if err != nil {
		return "", err
	}
	sm.run, sm.runLock = dir, lock
	return dir, nil
}

// Create makes a new originSession for repo, allocating its folder. Any
// existing session for the same repo is replaced (one per repo) in the same
// critical section, and retired.
func (sm *sessionManager) Create(repo, url string, auth authConfig) (*originSession, error) {
	run, err := sm.runDir()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(run, sessionDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("create session temp dir: %w", err)
	}

	sm.mu.Lock()
	if sm.closed {
		sm.mu.Unlock()
		removeChild(run, dir, sessionDirPrefix, "")
		return nil, errSessionsClosed
	}
	now := sm.now()
	s := &originSession{
		ID:         uuid.New().String(),
		Repo:       repo,
		URL:        url,
		Auth:       auth,
		TempDir:    dir,
		State:      stateCreated,
		CreatedAt:  now,
		LastAccess: now,
		runDir:     run,
		busy:       make(chan struct{}, 1),
		gone:       make(chan struct{}),
		onFinal:    sm.live.Done,
		now:        sm.now,
	}
	sm.live.Add(1)
	old := sm.sessions[repo]
	sm.sessions[repo] = s
	sm.ensureSweeperLocked()
	sm.mu.Unlock()

	if old != nil {
		old.retire()
		log.Debug().Str("repo", repo).Str("session_id", old.ID).Msg("origin session replaced")
	}
	log.Debug().Str("repo", repo).Str("session_id", s.ID).Str("tmp_dir", dir).Msg("origin session created")
	return s, nil
}

// Get retrieves the session for repo and verifies the ID matches.
// It touches LastAccess on a successful lookup, under mu, so a sweep either
// sees the touch or has already evicted the session and Get misses it.
// Returns (nil, false) if the repo has no session or the ID does not match.
// A caller that touches RemoteStore or TempDir uses Use instead.
func (sm *sessionManager) Get(repo, id string) (*originSession, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s := sm.sessions[repo]
	if s == nil || s.ID != id {
		return nil, false
	}
	s.mu.Lock()
	s.LastAccess = sm.now()
	s.mu.Unlock()
	return s, true
}

// Use is Get plus the session's use (USE on originSession), waiting for
// another holder as long as ctx allows. The caller must call release when it
// no longer touches the session's store or folder. It reports false when there
// is no such session, it was dropped while waiting, or ctx ended.
func (sm *sessionManager) Use(ctx context.Context, repo, id string) (s *originSession, release func(), ok bool) {
	s, ok = sm.Get(repo, id)
	if !ok {
		return nil, nil, false
	}
	release, ok = s.acquire(ctx)
	if !ok {
		return nil, nil, false
	}
	return s, release, true
}

// Delete removes the session for repo if its ID matches, and retires it.
func (sm *sessionManager) Delete(repo, id string) {
	sm.mu.Lock()
	s := sm.sessions[repo]
	if s == nil || s.ID != id {
		sm.mu.Unlock()
		return
	}
	delete(sm.sessions, repo)
	sm.mu.Unlock()

	s.retire()
	log.Debug().Str("repo", repo).Str("session_id", id).Msg("origin session deleted")
}

// ListByRepo returns all active sessions for the given repo name.
// The current model stores at most one session per repo, but the return type
// is a slice so callers don't need to know the storage layout.
func (sm *sessionManager) ListByRepo(repo string) []*originSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s := sm.sessions[repo]
	if s == nil {
		return nil
	}
	return []*originSession{s}
}

// ensureSweeperLocked starts the sweeper unless one is running. Create calls
// it under mu AFTER storing its session, and the sweeper decides to exit
// under mu too, so a session stored concurrently with that exit is always
// swept: the sweeper either sees it and keeps running, or has already
// cleared sweeping and this call starts a new one.
func (sm *sessionManager) ensureSweeperLocked() {
	if sm.sweeping {
		return
	}
	sm.sweeping = true
	go sm.cleanupLoop()
}

// cleanupLoop ticks every interval and removes sessions idle longer than
// sessionExpiry. It returns on the first tick after which no session is left,
// or when the manager's lifetime ends (see LIFETIME).
func (sm *sessionManager) cleanupLoop() {
	ticker := time.NewTicker(sm.interval)
	defer ticker.Stop()
	for {
		select {
		case <-sm.done:
			sm.mu.Lock()
			sm.sweeping = false
			sm.mu.Unlock()
			return
		case <-ticker.C:
		}
		sm.runCleanup()
		if sm.stopIfEmpty() {
			return
		}
	}
}

// stopIfEmpty clears sweeping and reports true when the manager holds no
// session; the check and the clear happen under mu (see ensureSweeperLocked).
func (sm *sessionManager) stopIfEmpty() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	empty := len(sm.sessions) == 0
	if empty {
		sm.sweeping = false
	}
	return empty
}

// runCleanup evicts the sessions idle longer than sessionExpiry. It decides
// and evicts under mu, so what it removes is exactly the session instance it
// found expired, never a newer one that has since taken the repo's key; it
// retires them after releasing mu.
func (sm *sessionManager) runCleanup() {
	deadline := sm.now().Add(-sessionExpiry)
	var expired []*originSession
	sm.mu.Lock()
	for repo, s := range sm.sessions {
		s.mu.Lock()
		idle := !s.inUse && s.LastAccess.Before(deadline) // a step in progress is not idle
		s.mu.Unlock()
		if idle {
			delete(sm.sessions, repo)
			expired = append(expired, s)
		}
	}
	sm.mu.Unlock()

	for _, s := range expired {
		s.retire()
		log.Debug().Str("repo", s.Repo).Str("session_id", s.ID).Msg("origin session expired")
	}
}

// close ends the manager's lifetime (LIFETIME). newSessionManager arranges
// for it to run when ctx ends; it is idempotent. It blocks until every
// session has finalized, which a handler still using one delays.
func (sm *sessionManager) close() {
	sm.mu.Lock()
	sm.closed = true
	all := sm.sessions
	sm.sessions = map[string]*originSession{}
	sm.mu.Unlock()

	for _, s := range all {
		s.retire()
	}
	sm.live.Wait()

	sm.runMu.Lock()
	sm.runClosed = true
	base, owned, run, lock := sm.base, sm.baseOwned, sm.run, sm.runLock
	sm.run, sm.runLock = "", nil
	sm.runMu.Unlock()

	if run != "" {
		removeChild(base, run, runPrefix, "")
		lockPath := lock.Name()
		_ = pki.UnlockFile(lock)
		_ = lock.Close()
		removeChild(base, lockPath, runPrefix, runLockSuffix)
	}
	if owned && strings.HasPrefix(filepath.Base(base), privateRootPattern) {
		if err := os.RemoveAll(base); err != nil {
			log.Warn().Err(err).Str("dir", base).Msg("origin sessions: failed to remove the sessions dir")
		}
	}
	if len(all) > 0 {
		log.Debug().Int("sessions", len(all)).Msg("origin sessions: removed at shutdown")
	}
}

// isChild reports whether p is a direct child of parent whose name starts
// with prefix and ends with suffix: the only shape removeChild removes.
func isChild(parent, p, prefix, suffix string) bool {
	if parent == "" || p == "" {
		return false
	}
	p = filepath.Clean(p)
	name := filepath.Base(p)
	return filepath.Dir(p) == filepath.Clean(parent) &&
		strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) &&
		len(name) > len(prefix)+len(suffix)
}

// removeChild removes p, and everything in it, if isChild(parent, p, prefix,
// suffix); it refuses, and logs, anything else. It reports whether p is gone.
func removeChild(parent, p, prefix, suffix string) bool {
	if !isChild(parent, p, prefix, suffix) {
		log.Warn().Str("path", p).Str("parent", parent).Msg("origin sessions: refusing to remove a path outside the session layout")
		return false
	}
	if err := os.RemoveAll(p); err != nil {
		log.Warn().Err(err).Str("path", p).Msg("origin sessions: removal failed")
		return false
	}
	return true
}
