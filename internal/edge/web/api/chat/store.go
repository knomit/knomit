package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// The chat's temp files (plan P12), under the manager's run directory:
//
//	<run>/<chatID>/transcript.jsonl      one line per transcript item
//	<run>/<chatID>/calls/<callID>.json   one tool call's arguments and result
//
// Directories are 0700 and files 0600. They die with the chat (end removes the
// chat directory) or with the run (the sweep). The persistent record is the
// session log, which this store never touches.

// callIDPattern is what a tool-call id may look like on disk and in a URL:
// the loop's ids ("tu_<n>") and nothing that could name another path.
var callIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// callRecord is the file one tool call leaves.
type callRecord struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	Elided    bool            `json:"elided"`
	// Title and KeyArg word the call's row, as its tool_call events do.
	Title  string `json:"title,omitempty"`
	KeyArg string `json:"key_arg,omitempty"`
	// InBase: the call's arguments carry binding, repo or lens (see viewItem).
	InBase bool `json:"in_base,omitempty"`
}

// chatStore is one chat's directory.
type chatStore struct {
	dir string

	mu         sync.Mutex
	transcript *os.File
}

// newChatStore makes <run>/<id> with its calls/ subdirectory and opens the
// transcript.
func newChatStore(run, id string) (*chatStore, error) {
	dir := filepath.Join(run, id)
	if err := os.MkdirAll(filepath.Join(dir, "calls"), 0o700); err != nil {
		return nil, fmt.Errorf("chat temp dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "transcript.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("chat transcript: %w", err)
	}
	return &chatStore{dir: dir, transcript: f}, nil
}

// appendTranscript writes one transcript line.
func (s *chatStore) appendTranscript(item any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transcript == nil {
		return
	}
	b, err := json.Marshal(struct {
		TS   string `json:"ts"`
		Item any    `json:"item"`
	}{time.Now().UTC().Format(time.RFC3339Nano), item})
	if err != nil {
		return
	}
	_, _ = s.transcript.Write(append(b, '\n'))
}

// putCall stores a tool call's record. The id has been through callIDPattern.
func (s *chatStore) putCall(id string, rec callRecord) error {
	if !callIDPattern.MatchString(id) {
		return fmt.Errorf("call id %q is not storable", id)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "calls", id+".json"), b, 0o600)
}

// getCall reads a stored call. os.ErrNotExist means there is none.
func (s *chatStore) getCall(id string) (callRecord, error) {
	var rec callRecord
	if !callIDPattern.MatchString(id) {
		return rec, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "calls", id+".json"))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, errors.New("stored call is unreadable")
	}
	return rec, nil
}

// remove closes the transcript and deletes the chat's directory.
func (s *chatStore) remove() {
	s.mu.Lock()
	if s.transcript != nil {
		_ = s.transcript.Close()
		s.transcript = nil
	}
	s.mu.Unlock()
	_ = os.RemoveAll(s.dir)
}
