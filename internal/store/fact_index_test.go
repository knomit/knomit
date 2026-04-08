package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	gomock "go.uber.org/mock/gomock"
)

// TestNotifyCommit_callsAppendCommitLog verifies that notifyCommit delegates to
// im.AppendCommitLog with the correct branch and hash, replacing the old appendLog
// function pointer.
func TestNotifyCommit_callsAppendCommitLog(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockIM := NewMockIndexManager(ctrl)
	branch := "agent/test"
	hash := plumbing.NewHash("abcdef1234567890abcdef1234567890abcdef12")

	mockIM.EXPECT().AppendCommitLog(gomock.Any(), branch, hash.String())

	fi := &factIndex{im: mockIM}
	fi.notifyCommit(context.Background(), branch, hash)
}

// TestNotifyCommit_callsExternalObserver verifies that the onCommit observer is
// still called after notifyCommit, independent of the IndexManager.
func TestNotifyCommit_callsExternalObserver(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockIM := NewMockIndexManager(ctrl)
	mockIM.EXPECT().AppendCommitLog(gomock.Any(), gomock.Any(), gomock.Any())

	called := false
	fi := &factIndex{
		im:       mockIM,
		onCommit: func(branch, hash string) { called = true },
	}
	fi.notifyCommit(context.Background(), "agent/test", plumbing.NewHash("abcdef1234567890abcdef1234567890abcdef12"))

	if !called {
		t.Error("expected onCommit observer to be called")
	}
}

// TestNotifyCommit_nilIM_doesNotPanic verifies that notifyCommit is safe when no
// IndexManager is set (e.g. when Service is used in DB-only mode).
func TestNotifyCommit_nilIM_doesNotPanic(t *testing.T) {
	fi := &factIndex{}
	fi.notifyCommit(context.Background(), "agent/test", plumbing.NewHash("abcdef1234567890abcdef1234567890abcdef12"))
}
