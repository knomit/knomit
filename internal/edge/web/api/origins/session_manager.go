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

// sessionDirPrefix names every per-session folder the manager makes under its
// root. Removal refuses any path that is not a direct child of the root with
// this prefix, so neither a stale-folder sweep nor a session cleanup can
// delete anything the manager did not create.
const sessionDirPrefix = "session-"

// privateRootPattern is the os.MkdirTemp pattern of the root a manager makes
// for itself when it is given none (see newSessionManager).
const privateRootPattern = "knomit-origin-sessions-"

// originSession holds ephemeral state for a single remote connection workflow.
// It is owned by the sessionManager and must not be shared across goroutines
// without holding mu.
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
// key. A folder is removed only after its session has left the map, outside
// mu: folder names are unique (os.MkdirTemp), so nothing in the map can
// refer to a folder being removed. Lock order is mu, then a session's mu.
//
// FOLDERS. Every session's folder is a direct child of root named
// sessionDirPrefix*, and removal refuses any other path (isSessionDir).
// root is a directory the manager owns: Mount's caller passes one per
// instance (web's Server puts it under the instance's home), and at
// construction the manager removes the session folders a previous run left
// there. Given no root, the manager makes a private one under os.TempDir()
// on its first Create and removes it when its lifetime ends; there is no
// previous run's to sweep.
//
// LIFETIME. The manager lives as long as the ctx it was built with. When ctx
// ends it removes every session and its folder, refuses further Creates, and
// its sweeper exits. The removal runs on its own goroutine (context.AfterFunc)
// and nothing waits for it, so a process that exits right after cancelling
// can leave folders behind; with a root, the next run's construction removes
// them. The idle-expiry sweeper is not a goroutine for the life of the
// manager: it runs only while the manager holds at least one session (Create
// starts it when none is running, and it exits on the first tick that finds
// the map empty), so a manager with no sessions owns no goroutine.
type sessionManager struct {
	now      func() time.Time // the clock expiry is judged by; time.Now outside tests
	interval time.Duration    // the sweeper's tick; cleanupInterval outside tests
	done     <-chan struct{}  // the manager's lifetime; nil never ends

	mu        sync.Mutex
	root      string                    // see FOLDERS; guarded by mu
	rootOwned bool                      // root is the private one this manager made
	sessions  map[string]*originSession // by repo name
	sweeping  bool                      // a sweeper goroutine is running
	closed    bool                      // the lifetime has ended
}

// newSessionManager creates an empty sessionManager over root ("" for a
// private one, see FOLDERS) that lives as long as ctx. It removes the session
// folders a previous run left in root, and starts no goroutine: the first
// Create does (see LIFETIME).
func newSessionManager(ctx context.Context, root string) *sessionManager {
	sm := &sessionManager{
		now:      time.Now,
		interval: cleanupInterval,
		done:     ctx.Done(),
		root:     root,
		sessions: map[string]*originSession{},
	}
	if root != "" {
		removeStale(root)
	}
	context.AfterFunc(ctx, sm.close)
	return sm
}

// removeStale removes the session folders a previous run left in root. It
// runs from newSessionManager, before the manager can hold a session, so
// every session folder in root belongs to a run that is over.
func removeStale(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn().Err(err).Str("dir", root).Msg("origin sessions: cannot list stale session folders")
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), sessionDirPrefix) {
			continue
		}
		removeSessionDir(root, filepath.Join(root, e.Name()))
		log.Info().Str("dir", e.Name()).Msg("origin sessions: removed a session folder left by a previous run")
	}
}

// Create makes a new originSession for repo, allocating its folder. Any
// existing session for the same repo is replaced (one per repo) in the same
// critical section, and its folder removed.
func (sm *sessionManager) Create(repo, url string, auth authConfig) (*originSession, error) {
	sm.mu.Lock()
	if sm.closed {
		sm.mu.Unlock()
		return nil, errSessionsClosed
	}
	dir, err := sm.makeSessionDirLocked()
	if err != nil {
		sm.mu.Unlock()
		return nil, err
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
	}
	old := sm.sessions[repo]
	sm.sessions[repo] = s
	root := sm.root
	sm.ensureSweeperLocked()
	sm.mu.Unlock()

	if old != nil {
		removeSessionDir(root, old.TempDir)
		log.Debug().Str("repo", repo).Str("session_id", old.ID).Msg("origin session replaced")
	}
	log.Debug().Str("repo", repo).Str("session_id", s.ID).Str("tmp_dir", dir).Msg("origin session created")
	return s, nil
}

// makeSessionDirLocked makes a new session folder under root, making root
// first (a private one if the manager has none). Callers hold mu.
func (sm *sessionManager) makeSessionDirLocked() (string, error) {
	if sm.root == "" {
		root, err := os.MkdirTemp("", privateRootPattern)
		if err != nil {
			return "", fmt.Errorf("create origin sessions dir: %w", err)
		}
		sm.root, sm.rootOwned = root, true
	} else if err := os.MkdirAll(sm.root, 0o700); err != nil {
		return "", fmt.Errorf("create origin sessions dir: %w", err)
	}
	dir, err := os.MkdirTemp(sm.root, sessionDirPrefix)
	if err != nil {
		return "", fmt.Errorf("create session temp dir: %w", err)
	}
	return dir, nil
}

// Get retrieves the session for repo and verifies the ID matches.
// It touches LastAccess on a successful lookup, under mu, so a sweep either
// sees the touch or has already evicted the session and Get misses it.
// Returns (nil, false) if the repo has no session or the ID does not match.
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

// Delete removes the session for repo if its ID matches, and its folder.
func (sm *sessionManager) Delete(repo, id string) {
	sm.mu.Lock()
	s := sm.sessions[repo]
	if s == nil || s.ID != id {
		sm.mu.Unlock()
		return
	}
	delete(sm.sessions, repo)
	root := sm.root
	sm.mu.Unlock()

	removeSessionDir(root, s.TempDir)
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
// found expired, never a newer one that has since taken the repo's key.
func (sm *sessionManager) runCleanup() {
	deadline := sm.now().Add(-sessionExpiry)
	var expired []*originSession
	sm.mu.Lock()
	for repo, s := range sm.sessions {
		s.mu.Lock()
		idle := s.LastAccess.Before(deadline)
		s.mu.Unlock()
		if idle {
			delete(sm.sessions, repo)
			expired = append(expired, s)
		}
	}
	root := sm.root
	sm.mu.Unlock()

	for _, s := range expired {
		removeSessionDir(root, s.TempDir)
		log.Debug().Str("repo", s.Repo).Str("session_id", s.ID).Msg("origin session expired")
	}
}

// close ends the manager's lifetime: it removes every session and its
// folder, the private root if the manager made one, and refuses further
// Creates. newSessionManager arranges for it to run when ctx ends; it is
// idempotent.
func (sm *sessionManager) close() {
	sm.mu.Lock()
	sm.closed = true
	all := sm.sessions
	sm.sessions = map[string]*originSession{}
	root, owned := sm.root, sm.rootOwned
	sm.mu.Unlock()

	for _, s := range all {
		removeSessionDir(root, s.TempDir)
	}
	if owned && strings.HasPrefix(filepath.Base(root), privateRootPattern) {
		if err := os.RemoveAll(root); err != nil {
			log.Warn().Err(err).Str("dir", root).Msg("origin sessions: failed to remove the sessions dir")
		}
	}
	if len(all) > 0 {
		log.Debug().Int("sessions", len(all)).Msg("origin sessions: removed at shutdown")
	}
}

// isSessionDir reports whether dir is a session folder of root: a direct
// child of it whose name has sessionDirPrefix.
func isSessionDir(root, dir string) bool {
	if root == "" || dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	return filepath.Dir(dir) == filepath.Clean(root) && strings.HasPrefix(filepath.Base(dir), sessionDirPrefix)
}

// removeSessionDir removes the session folder dir of root, logging any error.
// It refuses, and logs, any dir that is not a session folder of root.
func removeSessionDir(root, dir string) {
	if !isSessionDir(root, dir) {
		log.Warn().Str("dir", dir).Str("root", root).Msg("origin sessions: refusing to remove a path that is not a session folder")
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Warn().Err(err).Str("dir", dir).Msg("failed to remove session temp dir")
	}
}
