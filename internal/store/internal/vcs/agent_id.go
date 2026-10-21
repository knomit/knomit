package vcs

import (
	"errors"
	"sync/atomic"
	"testing"
)

// ErrNoAgentID is returned by every authored write on a Service that was not
// given this instance's agent id (SetAgentID). The id is the instance's
// PERSISTED identity (control.db instance_identity.agent_id); it is never
// derived from the name of the branch a commit lands on (D18: nothing infers
// an identity from a name). A store that does not know whose commits it is
// writing refuses to write them, as a store without a signer does.
var ErrNoAgentID = errors.New("store has no agent id: refusing to author a commit")

// OwnAgentID is the id an authored commit names: the id SetAgentID gave this
// store, else (inside a test binary only) the installed fallback's answer for
// branch, else ErrNoAgentID. branch is the AGENT branch the commit is
// authored as (for an experiment, its recorded parent); production never
// looks at it.
func (rh *Repo) OwnAgentID(branch string) (string, error) {
	if rh.agentID != "" {
		return rh.agentID, nil
	}
	if id := testFallbackAgentID(branch); id != "" {
		return id, nil
	}
	return "", ErrNoAgentID
}

// testFallbackAgent holds the function SetTestFallbackAgentID installed. It
// is read only inside a test binary; see testFallbackAgentID.
var testFallbackAgent atomic.Pointer[func(branch string) string]

// SetTestFallbackAgentID installs the agent id a store without SetAgentID
// authors as INSIDE A GO TEST BINARY, so the hundreds of fixtures that open a
// store (or build a repos.Manager) with only an agent branch keep working.
// The installer (package testsigner, from TestMain) decides what
// the fallback is; production code has no fallback at all. Pass nil to remove
// it.
//
// It panics outside a test binary, and testFallbackAgentID ignores it there
// too, so a production knomit can never author as a fallback id. Nothing
// outside this package reads the fallback: a caller that needs the id a
// fixture store authors as is given it explicitly (repos.Deps.AgentID,
// store.SetAgentID), so no production file reads a test seam.
func SetTestFallbackAgentID(f func(branch string) string) {
	if !testing.Testing() {
		panic("store.SetTestFallbackAgentID called outside a test binary")
	}
	if f == nil {
		testFallbackAgent.Store(nil)
		return
	}
	testFallbackAgent.Store(&f)
}

// testFallbackAgentID is the installed test fallback's id for branch, or ""
// outside a test binary or when none is installed: the READ side of
// SetTestFallbackAgentID, used by OwnAgentID alone.
func testFallbackAgentID(branch string) string {
	if !testing.Testing() {
		return ""
	}
	if p := testFallbackAgent.Load(); p != nil {
		return (*p)(branch)
	}
	return ""
}
