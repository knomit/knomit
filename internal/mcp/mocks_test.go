package mcp

import (
	"fmt"
	"strings"
)

// mockGitStore implements GitStore for tests.
type mockGitStore struct {
	written    map[string]string // path → content written by WriteFile/BatchWrite
	deleted    []string
	headHash   string
	tags       []string
	tagErr     error // if non-nil, Tag() returns this error
	syncResult SyncResult
	syncErr    error
	logEntries map[string][]LogEntry // path → log entries
	dirEntries map[string][]DirEntry // path → dir entries
	files      map[string]string     // pre-populated readable files
}

func newMockStore() *mockGitStore {
	return &mockGitStore{
		written:    map[string]string{},
		files:      map[string]string{},
		logEntries: map[string][]LogEntry{},
		dirEntries: map[string][]DirEntry{},
		headHash:   "abc123def456",
	}
}

func (m *mockGitStore) ReadFile(path string) (string, error) {
	if c, ok := m.files[path]; ok {
		return c, nil
	}
	if c, ok := m.written[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("ReadFile: not found: %s", path)
}

func (m *mockGitStore) WriteFile(path, content, message string) error {
	m.written[path] = content
	return nil
}

func (m *mockGitStore) BatchWrite(files map[string]string, message string) error {
	for k, v := range files {
		m.written[k] = v
	}
	return nil
}

func (m *mockGitStore) DeleteFile(path, message string) error {
	m.deleted = append(m.deleted, path)
	return nil
}

func (m *mockGitStore) FileExists(path string) (bool, error) {
	if _, ok := m.files[path]; ok {
		return true, nil
	}
	if _, ok := m.written[path]; ok {
		return true, nil
	}
	return false, nil
}

func (m *mockGitStore) ListDir(path string) ([]DirEntry, error) {
	if entries, ok := m.dirEntries[path]; ok {
		return entries, nil
	}
	return []DirEntry{}, nil
}

func (m *mockGitStore) ListAll() ([]string, error) {
	var paths []string
	for p := range m.files {
		if strings.HasSuffix(p, ".md") {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func (m *mockGitStore) Log(path string) ([]LogEntry, error) {
	if entries, ok := m.logEntries[path]; ok {
		return entries, nil
	}
	return []LogEntry{}, nil
}

func (m *mockGitStore) Grep(pattern string) ([]string, error) {
	return nil, nil
}

func (m *mockGitStore) DiffFiles(fromCommit string) (added, modified, deleted []string, err error) {
	return nil, nil, nil, nil
}

func (m *mockGitStore) HeadCommit() (string, error) {
	return m.headHash, nil
}

func (m *mockGitStore) Tag(name string) error {
	if m.tagErr != nil {
		return m.tagErr
	}
	m.tags = append(m.tags, name)
	return nil
}

func (m *mockGitStore) Sync(remoteAuth interface{}) (SyncResult, error) {
	return m.syncResult, m.syncErr
}

func (m *mockGitStore) TagsContaining(hash string) ([]string, error) {
	return m.tags, nil
}

func (m *mockGitStore) Branch() string {
	return "agent/test"
}

// mockIndex implements SearchIndex for tests.
type mockIndex struct {
	upserted []FactRecord
	deleted  []string
	results  []SearchResult
	searchErr error
}

func (m *mockIndex) Search(q SearchQuery) ([]SearchResult, error) {
	return m.results, m.searchErr
}

func (m *mockIndex) Upsert(r FactRecord) error {
	m.upserted = append(m.upserted, r)
	return nil
}

func (m *mockIndex) Delete(path string) error {
	m.deleted = append(m.deleted, path)
	return nil
}

func (m *mockIndex) Sync(g GitReader) error {
	return nil
}

func (m *mockIndex) GetLastCommit() (string, error) {
	return "", nil
}

func (m *mockIndex) SetLastCommit(hash string) error {
	return nil
}
