package web

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"knomit/internal/store"
)

// AuthConfig holds authentication credentials for a remote connection attempt.
type AuthConfig struct {
	Method   string // "token", "basic", "ssh"
	Token    string
	User     string
	Password string
}

// SessionState represents the lifecycle stage of an OriginSession.
type SessionState string

const (
	StateCreated   SessionState = "created"
	StateTested    SessionState = "tested"
	StatePreviewed SessionState = "previewed"
	StateApplied   SessionState = "applied"
	StateCommitted SessionState = "committed"
	StateError     SessionState = "error"
)

// sessionExpiry is the idle duration after which a session is eligible for cleanup.
const sessionExpiry = 10 * time.Minute

// cleanupInterval is how often the background goroutine checks for expired sessions.
const cleanupInterval = 60 * time.Second

// OriginSession holds ephemeral state for a single remote connection workflow.
// It is owned by the SessionManager and must not be shared across goroutines
// without holding mu.
type OriginSession struct {
	ID          string
	Repo        string
	URL         string
	Auth        AuthConfig
	TempDir     string
	State       SessionState
	CreatedAt   time.Time
	LastAccess  time.Time
	RemoteStore   *store.Service  // cloned remote store, set by test handler
	RemoteBranch  string     // remote branch to track, set by apply handler
	AppliedBranch string     // agent branch written into the clone during apply; used by commit for rebuild
	TestResult    any        // cached result from test step
	PreviewResult any        // cached result from preview step
	ApplyResult   any        // cached result from apply step
	mu sync.Mutex
}

// SessionManager manages one ephemeral OriginSession per repo.
// Sessions are stored in a sync.Map keyed by repo name.
type SessionManager struct {
	sessions sync.Map // map[repoName]*OriginSession
	done     chan struct{}
}

// NewSessionManager creates a SessionManager and starts the background
// cleanup goroutine.
func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		done: make(chan struct{}),
	}
	go sm.cleanupLoop()
	return sm
}

// Create makes a new OriginSession for repo, allocating a temp directory.
// Any existing session for the same repo is cleaned up first (one per repo).
func (sm *SessionManager) Create(repo, url string, auth AuthConfig) (*OriginSession, error) {
	// Remove any existing session for this repo.
	sm.deleteByRepo(repo)

	tmpDir, err := os.MkdirTemp(os.TempDir(), "knomit-origin-")
	if err != nil {
		return nil, fmt.Errorf("create session temp dir: %w", err)
	}

	now := time.Now()
	s := &OriginSession{
		ID:         uuid.New().String(),
		Repo:       repo,
		URL:        url,
		Auth:       auth,
		TempDir:    tmpDir,
		State:      StateCreated,
		CreatedAt:  now,
		LastAccess: now,
	}

	sm.sessions.Store(repo, s)
	log.Debug().Str("repo", repo).Str("session_id", s.ID).Str("tmp_dir", tmpDir).Msg("origin session created")
	return s, nil
}

// Get retrieves the session for repo and verifies the ID matches.
// It touches LastAccess on a successful lookup.
// Returns (nil, false) if the repo has no session or the ID does not match.
func (sm *SessionManager) Get(repo, id string) (*OriginSession, bool) {
	v, ok := sm.sessions.Load(repo)
	if !ok {
		return nil, false
	}
	s := v.(*OriginSession)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ID != id {
		return nil, false
	}
	s.LastAccess = time.Now()
	return s, true
}

// Delete removes the session for repo if its ID matches and cleans up the temp dir.
func (sm *SessionManager) Delete(repo, id string) {
	v, ok := sm.sessions.Load(repo)
	if !ok {
		return
	}
	s := v.(*OriginSession)
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

// Shutdown stops the cleanup goroutine and removes all active sessions.
func (sm *SessionManager) Shutdown() {
	close(sm.done)
	sm.sessions.Range(func(k, v any) bool {
		s := v.(*OriginSession)
		sm.sessions.Delete(k)
		removeTempDir(s.TempDir)
		return true
	})
	log.Debug().Msg("session manager shut down")
}

// deleteByRepo removes the session for repo unconditionally (no ID check).
func (sm *SessionManager) deleteByRepo(repo string) {
	v, ok := sm.sessions.LoadAndDelete(repo)
	if !ok {
		return
	}
	s := v.(*OriginSession)
	removeTempDir(s.TempDir)
	log.Debug().Str("repo", repo).Str("session_id", s.ID).Msg("origin session replaced")
}

// cleanupLoop ticks every cleanupInterval and removes sessions idle longer than sessionExpiry.
func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sm.done:
			return
		case <-ticker.C:
			sm.runCleanup()
		}
	}
}

// runCleanup iterates all sessions and evicts those that have been idle too long.
func (sm *SessionManager) runCleanup() {
	deadline := time.Now().Add(-sessionExpiry)
	sm.sessions.Range(func(k, v any) bool {
		s := v.(*OriginSession)
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
