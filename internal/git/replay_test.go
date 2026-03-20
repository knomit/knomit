package git_test

import (
	"database/sql"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	git "knomit/internal/git"
	"knomit/internal/store"
	storegit "knomit/internal/store/git"
)

// factsSchema is the minimal schema needed for the facts table used by FactsIter.
const factsSchema = `
CREATE TABLE IF NOT EXISTS facts (
    path        TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    blob_hash   TEXT NOT NULL,
    type        TEXT NOT NULL DEFAULT 'observation',
    domain      TEXT NOT NULL,
    entities    TEXT NOT NULL,
    confidence  REAL NOT NULL,
    sources     INTEGER NOT NULL,
    refs        TEXT NOT NULL,
    commit_hash TEXT NOT NULL
);
`

// newTestStorerForReplay creates a storer with both git and facts schemas.
func newTestStorerForReplay(t *testing.T) (*storegit.Storer, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	schema := `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS commit_log (commit_hash TEXT NOT NULL, path TEXT NOT NULL, committed_at INTEGER NOT NULL, message TEXT NOT NULL, operation TEXT NOT NULL DEFAULT '', author_email TEXT NOT NULL DEFAULT '', action TEXT NOT NULL DEFAULT '', PRIMARY KEY (commit_hash, path));
` + factsSchema
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return storegit.NewStorer(db), db
}

// storeIterAdapter wraps store.FactsIter to implement git.FactIter.
type storeIterAdapter struct {
	inner *store.FactsIter
}

func (a *storeIterAdapter) Next() (*git.FactRow, error) {
	row, err := a.inner.Next()
	if err != nil || row == nil {
		return nil, err
	}
	return &git.FactRow{Path: row.Path, BlobHash: row.BlobHash, CommitHash: row.CommitHash}, nil
}

func (a *storeIterAdapter) Close() error { return a.inner.Close() }

func mustNewIter(t *testing.T, db *sql.DB) git.FactIter {
	t.Helper()
	iter, err := store.NewFactsIter(db)
	if err != nil {
		t.Fatal(err)
	}
	return &storeIterAdapter{inner: iter}
}

// insertFact inserts a row into the facts table for the iterator.
func insertFact(t *testing.T, db *sql.DB, path, blobHash, commitHash string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT OR REPLACE INTO facts (path, title, blob_hash, type, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES (?, ?, ?, 'observation', '[]', '[]', 0.9, 1, '[]', ?)`,
		path, "title", blobHash, commitHash,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReplay_CopiesFactsToTempBranch(t *testing.T) {
	// Create local store with 3 facts.
	localStorer, localDB := newTestStorerForReplay(t)
	local, err := git.InitWithStorer(localStorer, nil, "agent/local")
	if err != nil {
		t.Fatal(err)
	}

	facts := map[string]string{
		"kb/fact1.md": "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Fact 1\n\nBody 1.\n",
		"kb/fact2.md": "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Fact 2\n\nBody 2.\n",
		"kb/fact3.md": "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Fact 3\n\nBody 3.\n",
	}

	for path, content := range facts {
		commitHash, blobHash, err := local.WriteFile(path, content, "add "+path, "learn")
		if err != nil {
			t.Fatal(err)
		}
		insertFact(t, localDB, path, blobHash, commitHash)
	}

	// Create empty target store.
	targetStorer, _ := newTestStorerForReplay(t)
	target, err := git.InitWithStorer(targetStorer, nil, "agent/target-placeholder")
	if err != nil {
		t.Fatal(err)
	}

	// Replay local facts into target.
	cfg := git.ReplayConfig{
		Strategy:      git.StrategyLocalWins,
		AgentBranch:   "agent/replay-test",
		DefaultBranch: "main",
	}
	result, err := git.Replay(local, mustNewIter(t, localDB), target, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.FromLocal != 3 {
		t.Fatalf("FromLocal = %d, want 3", result.FromLocal)
	}

	// Verify all 3 facts exist in target.
	for path := range facts {
		content, err := target.ReadFile(path)
		if err != nil {
			t.Fatalf("expected %s in target, got error: %v", path, err)
		}
		if content == "" {
			t.Fatalf("expected non-empty content for %s", path)
		}
	}

	// Verify target is on the replay branch.
	if target.Branch() != "agent/replay-test" {
		t.Fatalf("target branch = %q, want %q", target.Branch(), "agent/replay-test")
	}
}

func TestReplay_LocalWins_OverwritesSharedPath(t *testing.T) {
	// Create local store with a fact.
	localStorer, localDB := newTestStorerForReplay(t)
	local, err := git.InitWithStorer(localStorer, nil, "agent/local")
	if err != nil {
		t.Fatal(err)
	}

	localContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Version\n\nLocal body.\n"
	commitHash, blobHash, err := local.WriteFile("kb/shared.md", localContent, "add shared", "learn")
	if err != nil {
		t.Fatal(err)
	}
	insertFact(t, localDB, "kb/shared.md", blobHash, commitHash)

	// Create target store with the same path but different content.
	targetStorer, _ := newTestStorerForReplay(t)
	target, err := git.InitWithStorer(targetStorer, nil, "agent/target-placeholder")
	if err != nil {
		t.Fatal(err)
	}

	remoteContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Version\n\nRemote body.\n"
	if _, _, err := target.WriteFile("kb/shared.md", remoteContent, "add shared remote", "learn"); err != nil {
		t.Fatal(err)
	}
	// Advance main to HEAD so that the agent branch created from main has the file.
	advanceMainToHead(t, target)

	cfg := git.ReplayConfig{
		Strategy:      git.StrategyLocalWins,
		AgentBranch:   "agent/replay-test",
		DefaultBranch: "main",
	}
	result, err := git.Replay(local, mustNewIter(t, localDB), target, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Overwrites != 1 {
		t.Fatalf("Overwrites = %d, want 1", result.Overwrites)
	}

	// The target should now have local content.
	content, err := target.ReadFile("kb/shared.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != localContent {
		t.Fatalf("expected local content, got: %q", content)
	}
}

func TestReplay_RemoteWins_KeepsRemoteOnSharedPath(t *testing.T) {
	// Create local store with a fact.
	localStorer, localDB := newTestStorerForReplay(t)
	local, err := git.InitWithStorer(localStorer, nil, "agent/local")
	if err != nil {
		t.Fatal(err)
	}

	localContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Version\n\nLocal body.\n"
	commitHash, blobHash, err := local.WriteFile("kb/shared.md", localContent, "add shared", "learn")
	if err != nil {
		t.Fatal(err)
	}
	insertFact(t, localDB, "kb/shared.md", blobHash, commitHash)

	// Create target store with the same path but different content.
	targetStorer, _ := newTestStorerForReplay(t)
	target, err := git.InitWithStorer(targetStorer, nil, "agent/target-placeholder")
	if err != nil {
		t.Fatal(err)
	}

	remoteContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Remote Version\n\nRemote body.\n"
	if _, _, err := target.WriteFile("kb/shared.md", remoteContent, "add shared remote", "learn"); err != nil {
		t.Fatal(err)
	}
	advanceMainToHead(t, target)

	cfg := git.ReplayConfig{
		Strategy:      git.StrategyRemoteWins,
		AgentBranch:   "agent/replay-test",
		DefaultBranch: "main",
	}
	result, err := git.Replay(local, mustNewIter(t, localDB), target, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Overwrites != 0 {
		t.Fatalf("Overwrites = %d, want 0", result.Overwrites)
	}
	if result.FromLocal != 0 {
		t.Fatalf("FromLocal = %d, want 0 (remote wins, so local should be skipped)", result.FromLocal)
	}

	// The target should still have remote content.
	content, err := target.ReadFile("kb/shared.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != remoteContent {
		t.Fatalf("expected remote content, got: %q", content)
	}
}

func TestReplay_ResolvesDeadRefs(t *testing.T) {
	// Create local store.
	localStorer, localDB := newTestStorerForReplay(t)
	local, err := git.InitWithStorer(localStorer, nil, "agent/local")
	if err != nil {
		t.Fatal(err)
	}

	// Write fact B with an external URL ref.
	factBContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: [https://example.com/source]\n---\n# Fact B\n\nBody B.\n"
	if _, _, err := local.WriteFile("kb/fact-b.md", factBContent, "add fact B", "learn"); err != nil {
		t.Fatal(err)
	}

	// Delete fact B.
	if _, err := local.DeleteFile("kb/fact-b.md", "delete fact B", "retract"); err != nil {
		t.Fatal(err)
	}

	// Write fact A that refs the now-deleted fact B.
	factAContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: [kb/fact-b.md]\n---\n# Fact A\n\nBody A.\n"
	commitHash, blobHash, err := local.WriteFile("kb/fact-a.md", factAContent, "add fact A", "learn")
	if err != nil {
		t.Fatal(err)
	}
	insertFact(t, localDB, "kb/fact-a.md", blobHash, commitHash)

	// Create empty target store.
	targetStorer, _ := newTestStorerForReplay(t)
	target, err := git.InitWithStorer(targetStorer, nil, "agent/target-placeholder")
	if err != nil {
		t.Fatal(err)
	}

	cfg := git.ReplayConfig{
		Strategy:      git.StrategyLocalWins,
		AgentBranch:   "agent/replay-test",
		DefaultBranch: "main",
	}
	result, err := git.Replay(local, mustNewIter(t, localDB), target, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.RefsResolvedFromHist != 1 {
		t.Fatalf("RefsResolvedFromHist = %d, want 1", result.RefsResolvedFromHist)
	}

	// Verify fact A now has the external URL from fact B.
	content, err := target.ReadFile("kb/fact-a.md")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(content, "https://example.com/source") {
		t.Fatalf("expected grafted URL ref in fact A, got: %q", content)
	}
	if contains(content, "kb/fact-b.md") {
		t.Fatalf("expected dead local ref to be removed from fact A, got: %q", content)
	}
}

func TestReplay_DropsOrphanDeadRefs(t *testing.T) {
	// Create local store.
	localStorer, localDB := newTestStorerForReplay(t)
	local, err := git.InitWithStorer(localStorer, nil, "agent/local")
	if err != nil {
		t.Fatal(err)
	}

	// Write fact B with NO external refs.
	factBContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Fact B\n\nBody B with no external refs.\n"
	if _, _, err := local.WriteFile("kb/fact-b.md", factBContent, "add fact B", "learn"); err != nil {
		t.Fatal(err)
	}

	// Delete fact B.
	if _, err := local.DeleteFile("kb/fact-b.md", "delete fact B", "retract"); err != nil {
		t.Fatal(err)
	}

	// Write fact A that refs the now-deleted fact B.
	factAContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: [kb/fact-b.md]\n---\n# Fact A\n\nBody A.\n"
	commitHash, blobHash, err := local.WriteFile("kb/fact-a.md", factAContent, "add fact A", "learn")
	if err != nil {
		t.Fatal(err)
	}
	insertFact(t, localDB, "kb/fact-a.md", blobHash, commitHash)

	// Create empty target store.
	targetStorer, _ := newTestStorerForReplay(t)
	target, err := git.InitWithStorer(targetStorer, nil, "agent/target-placeholder")
	if err != nil {
		t.Fatal(err)
	}

	cfg := git.ReplayConfig{
		Strategy:      git.StrategyLocalWins,
		AgentBranch:   "agent/replay-test",
		DefaultBranch: "main",
	}
	result, err := git.Replay(local, mustNewIter(t, localDB), target, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.DanglingRefsDropped != 1 {
		t.Fatalf("DanglingRefsDropped = %d, want 1", result.DanglingRefsDropped)
	}

	// Verify fact A has empty refs.
	content, err := target.ReadFile("kb/fact-a.md")
	if err != nil {
		t.Fatal(err)
	}
	if contains(content, "kb/fact-b.md") {
		t.Fatalf("expected dead ref to be dropped from fact A, got: %q", content)
	}
}

func TestReplay_UsesExistingAgentBranch(t *testing.T) {
	// Create local store with a fact.
	localStorer, localDB := newTestStorerForReplay(t)
	local, err := git.InitWithStorer(localStorer, nil, "agent/local")
	if err != nil {
		t.Fatal(err)
	}

	localContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Local Fact\n\nLocal body.\n"
	commitHash, blobHash, err := local.WriteFile("kb/local.md", localContent, "add local", "learn")
	if err != nil {
		t.Fatal(err)
	}
	insertFact(t, localDB, "kb/local.md", blobHash, commitHash)

	// Create target store that already has an agent branch with content.
	targetStorer, _ := newTestStorerForReplay(t)
	target, err := git.InitWithStorer(targetStorer, nil, "agent/replay-test")
	if err != nil {
		t.Fatal(err)
	}

	// Write a fact on the agent branch that only exists there (not on main).
	agentContent := "---\ntype: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Agent Fact\n\nFrom agent branch.\n"
	if _, _, err := target.WriteFile("kb/agent-existing.md", agentContent, "add agent fact", "learn"); err != nil {
		t.Fatal(err)
	}

	// Replay: should detect existing agent branch and replay on top of it.
	cfg := git.ReplayConfig{
		Strategy:          git.StrategyLocalWins,
		AgentBranch:       "agent/replay-test",
		DefaultBranch:     "main",
		UseExistingBranch: true,
	}
	result, err := git.Replay(local, mustNewIter(t, localDB), target, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.FromLocal != 1 {
		t.Fatalf("FromLocal = %d, want 1", result.FromLocal)
	}

	// The local fact should exist.
	content, err := target.ReadFile("kb/local.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != localContent {
		t.Fatalf("expected local content, got: %q", content)
	}

	// The pre-existing agent fact should still be there.
	content, err = target.ReadFile("kb/agent-existing.md")
	if err != nil {
		t.Fatalf("expected agent-existing.md to still exist, got error: %v", err)
	}
	if content != agentContent {
		t.Fatalf("expected agent content preserved, got: %q", content)
	}
}

// advanceMainToHead sets the main branch ref to the current HEAD commit.
func advanceMainToHead(t *testing.T, s *git.Store) {
	t.Helper()
	head, err := s.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Storer().SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(head)),
	); err != nil {
		t.Fatal(err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && containsStr(s, substr)))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Verify FactsIter is used correctly (compile-time check).
var _ = store.NewFactsIter
