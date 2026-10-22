package store

import (
	"context"

	"knomit/internal/engine/store/index"

	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// Tamper helpers used only by this package's own tests. They live in a
// _test.go file so no production binary carries a raw-SQL escape hatch.

// RawExecutableForTest commits an EXECUTABLE file (mode 100755) at path,
// case kept — what `git add` of a +x file records. EXISTS ONLY for tests that
// an executable file under .knomit/ is still a file; knomit's writers only
// write mode 100644.
func (s *Service) RawExecutableForTest(ctx context.Context, branch, path, content, message string) (string, error) {
	return s.rh.RawEntryForTest(ctx, branch, path, content, message, filemode.Executable)
}

// deleteBranchFactsRowForTest removes a branch_facts row for (branch, path).
// Test-only escape hatch for integrity-check tests.
func (s *Service) deleteBranchFactsRowForTest(branch, path string) error {
	_, err := s.rh.Storer().DB().Exec(
		`DELETE FROM branch_facts
		 WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND path = ?`,
		branch, path)
	return err
}

// corruptFactsBlobHashForTest overwrites the blob_hash column on a facts row
// so it no longer matches the tree's blob hash at HEAD. Test-only escape hatch.
func (s *Service) corruptFactsBlobHashForTest(factID int64, newBlobHash string) error {
	_, err := s.rh.Storer().DB().Exec(
		`UPDATE facts SET blob_hash = ? WHERE id = ?`, newBlobHash, factID)
	return err
}

// deleteEmbeddingForTest removes a facts_vec row for the given facts.id.
// Test-only escape hatch.
func (s *Service) deleteEmbeddingForTest(factID int64) error {
	_, err := s.rh.Storer().DB().Exec(`DELETE FROM facts_vec WHERE rowid = ?`, factID)
	return err
}

// deleteGraphFactNodeForTest removes a Fact node from the graph for the given
// (path, blob_hash). Test-only escape hatch for integrity tests. Incident
// edges, labels and properties cascade (ON DELETE CASCADE), which is what
// Cypher's DETACH DELETE did.
func (s *Service) deleteGraphFactNodeForTest(path, blobHash string) error {
	_, err := s.rh.Storer().DB().Exec(`
		DELETE FROM nodes
		WHERE id IN (
			SELECT nl.node_id
			FROM node_labels nl
			JOIN node_props_text p ON p.node_id = nl.node_id
			JOIN property_keys kp ON kp.id = p.key_id AND kp.key = 'path'
			JOIN node_props_text b ON b.node_id = nl.node_id
			JOIN property_keys kb ON kb.id = b.key_id AND kb.key = 'blob_hash'
			WHERE nl.label = ? AND p.value = ? AND b.value = ?
		)`, index.NodeFact, path, blobHash)
	return err
}
