package origins

import (
	"fmt"
	"os"
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

// sessionManager manages one ephemeral originSession per repo.
// Sessions are stored in a sync.Map keyed by repo name. Mount creates the one
// manager its routes share; nothing outside this package can reach it.
//
// LIFETIME. The idle-expiry sweeper is not a goroutine for the life of the
// manager. It runs only while the manager holds at least one session: Create
// starts it when none is running, and it exits on the first tick that finds
// the map empty. A manager with no sessions therefore owns no goroutine, so
// one that is dropped (a test's router, a rebuilt handler) leaks nothing, and
// there is no Shutdown for anyone to forget to call. A session still held
// when the process exits keeps its temp dir under os.TempDir(), exactly as
// before: nothing in production ever called the old Shutdown.
type sessionManager struct {
	sessions sync.Map // map[repoName]*originSession

	sweepMu  sync.Mutex
	sweeping bool // a sweeper goroutine is running; guarded by sweepMu
}

// newSessionManager creates an empty sessionManager. It starts no goroutine:
// the first Create does (see the LIFETIME note on sessionManager).
func newSessionManager() *sessionManager {
	return &sessionManager{}
}

// Create makes a new originSession for repo, allocating a temp directory.
// Any existing session for the same repo is cleaned up first (one per repo).
func (sm *sessionManager) Create(repo, url string, auth authConfig) (*originSession, error) {
	// Remove any existing session for this repo.
	sm.deleteByRepo(repo)

	tmpDir, err := os.MkdirTemp(os.TempDir(), "knomit-origin-")
	if err != nil {
		return nil, fmt.Errorf("create session temp dir: %w", err)
	}

	now := time.Now()
	s := &originSession{
		ID:         uuid.New().String(),
		Repo:       repo,
		URL:        url,
		Auth:       auth,
		TempDir:    tmpDir,
		State:      stateCreated,
		CreatedAt:  now,
		LastAccess: now,
	}

	sm.sessions.Store(repo, s)
	sm.ensureSweeper()
	log.Debug().Str("repo", repo).Str("session_id", s.ID).Str("tmp_dir", tmpDir).Msg("origin session created")
	return s, nil
}

// Get retrieves the session for repo and verifies the ID matches.
// It touches LastAccess on a successful lookup.
// Returns (nil, false) if the repo has no session or the ID does not match.
func (sm *sessionManager) Get(repo, id string) (*originSession, bool) {
	v, ok := sm.sessions.Load(repo)
	if !ok {
		return nil, false
	}
	s := v.(*originSession)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ID != id {
		return nil, false
	}
	s.LastAccess = time.Now()
	return s, true
}

// Delete removes the session for repo if its ID matches and cleans up the temp dir.
func (sm *sessionManager) Delete(repo, id string) {
	v, ok := sm.sessions.Load(repo)
	if !ok {
		return
	}
	s := v.(*originSession)
	s.mu.Lock()
	if s.ID != id {
		s.mu.Unlock()
		return
	}
	tmpDir := s.TempDir
	s.mu.Unlock()

	sm.sessions.Delete(repo)
	removeTempDir(tmpDir)
	log.Debug().Str("repo", repo).Str("session_id", id).Msg("origin session deleted")
}

// ListByRepo returns all active sessions for the given repo name.
// The current model stores at most one session per repo, but the return type
// is a slice so callers don't need to know the storage layout.
func (sm *sessionManager) ListByRepo(repo string) []*originSession {
	v, ok := sm.sessions.Load(repo)
	if !ok {
		return nil
	}
	return []*originSession{v.(*originSession)}
}

// deleteByRepo removes the session for repo unconditionally (no ID check).
func (sm *sessionManager) deleteByRepo(repo string) {
	v, ok := sm.sessions.LoadAndDelete(repo)
	if !ok {
		return
	}
	s := v.(*originSession)
	removeTempDir(s.TempDir)
	log.Debug().Str("repo", repo).Str("session_id", s.ID).Msg("origin session replaced")
}

// ensureSweeper starts the sweeper unless one is running. Create calls it
// AFTER storing its session, and the sweeper decides to exit under the same
// lock, so a session stored concurrently with that exit is always swept: the
// sweeper either sees it and keeps running, or has already cleared sweeping
// and this call starts a new one.
func (sm *sessionManager) ensureSweeper() {
	sm.sweepMu.Lock()
	defer sm.sweepMu.Unlock()
	if sm.sweeping {
		return
	}
	sm.sweeping = true
	go sm.cleanupLoop()
}

// cleanupLoop ticks every cleanupInterval and removes sessions idle longer
// than sessionExpiry. It returns on the first tick after which no session is
// left (see the LIFETIME note on sessionManager).
func (sm *sessionManager) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		sm.runCleanup()
		if sm.stopIfEmpty() {
			return
		}
	}
}

// stopIfEmpty clears sweeping and reports true when the manager holds no
// session; the check and the clear happen under sweepMu (see ensureSweeper).
func (sm *sessionManager) stopIfEmpty() bool {
	sm.sweepMu.Lock()
	defer sm.sweepMu.Unlock()
	empty := true
	sm.sessions.Range(func(any, any) bool {
		empty = false
		return false
	})
	if empty {
		sm.sweeping = false
	}
	return empty
}

// runCleanup iterates all sessions and evicts those that have been idle too long.
func (sm *sessionManager) runCleanup() {
	deadline := time.Now().Add(-sessionExpiry)
	sm.sessions.Range(func(k, v any) bool {
		s := v.(*originSession)
		s.mu.Lock()
		expired := s.LastAccess.Before(deadline)
		tmpDir := s.TempDir
		id := s.ID
		s.mu.Unlock()

		if expired {
			sm.sessions.Delete(k)
			removeTempDir(tmpDir)
			log.Debug().Str("repo", k.(string)).Str("session_id", id).Msg("origin session expired")
		}
		return true
	})
}

// removeTempDir removes a temp directory, logging any error.
func removeTempDir(dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Warn().Err(err).Str("dir", dir).Msg("failed to remove session temp dir")
	}
}
