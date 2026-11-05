package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/facts"
)

// #429 decision 1: no fact is written with its validations skipped. An
// unparseable (or empty) ontology at the branch's tip refuses every fact
// write at validateWrite — it used to let them through unjudged — and the
// repo's gate (SetOntologyUnusable) refuses every fact write whatever the
// tip holds. Neither stops a write that touches no fact: the ontology's own
// repair.
//
// SABOTAGE: restore validateWrite's `return nil` on a parse failure → the
// garbled and empty rows land → red; drop the OntologyUnusable check → the
// gate row lands → red.
func TestValidateWrite_UnusableOntologyRefusesEveryFactWrite(t *testing.T) {
	ctx := context.Background()
	const branch = "agent/test"
	fresh := func(t *testing.T, ontology string) *Service {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		require.NoError(t, svc.InitRepo(ctx, map[string]string{fact.OntologyFile: codeOntologyYAML(t)}, branch))
		_, err = svc.Facts().WriteFact(ctx, branch, fact.OntologyFile, ontology, "set the ontology", "updated")
		require.NoError(t, err, "a write that touches no fact is never refused")
		return svc
	}
	good := principle(t, "a principle", true)

	for name, ont := range map[string]string{"garbled": "topics: [::\n", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			svc := fresh(t, ont)
			_, err := svc.Facts().WriteFact(ctx, branch, "kb/principles/ui/new.md", good, "w", "learn")
			require.ErrorIs(t, err, facts.ErrOntologyUnusable, "a fact is never written unjudged")
			// The repair lands, and then the write does.
			_, err = svc.Facts().WriteFact(ctx, branch, fact.OntologyFile, codeOntologyYAML(t), "repair", "updated")
			require.NoError(t, err)
			_, err = svc.Facts().WriteFact(ctx, branch, "kb/principles/ui/new.md", good, "w", "learn")
			require.NoError(t, err)
		})
	}

	// The repo's gate refuses EVERY fact mutation, not only content writes:
	// a delete (a REST delete, a review prune, a dedup loser), a batch
	// retract, an empty-content write and a WithCarriedOver (byte-identical
	// replay) write. The ontology file's own repair passes.
	//
	// SABOTAGE: drop the gate from deleteFile → the delete row lands → red;
	// from batchWriteLocked → the retract and batch rows land → red; move it
	// after the carriedOver exemption in writeFileExact → the carried-over
	// row lands → red; drop it from writeFileExact → the write rows land →
	// red.
	t.Run("gate", func(t *testing.T) {
		svc := fresh(t, codeOntologyYAML(t))
		const existing = "kb/principles/ui/existing.md"
		_, err := svc.Facts().WriteFact(ctx, branch, existing, good, "seed", "learn")
		require.NoError(t, err)
		head := func() string {
			h, err := svc.HeadCommit(ctx, branch)
			require.NoError(t, err)
			return h
		}

		svc.SetOntologyUnusable(errors.New("no ontology at .knomit/ontology.yaml"))
		before := head()
		for name, mutate := range map[string]func() error{
			"write": func() error {
				_, err := svc.Facts().WriteFact(ctx, branch, "kb/principles/ui/new.md", good, "w", "learn")
				return err
			},
			"empty-content write": func() error {
				_, err := svc.Facts().WriteFact(ctx, branch, "kb/principles/ui/empty.md", "", "w", "learn")
				return err
			},
			"carried-over write": func() error {
				_, err := svc.Facts().WriteFact(facts.WithCarriedOver(ctx), branch, "kb/principles/ui/copy.md", good, "replay", "replay")
				return err
			},
			"delete": func() error {
				_, err := svc.Facts().DeleteFact(ctx, branch, existing, "prune")
				return err
			},
			"retract (batch delete)": func() error {
				_, _, err := svc.Facts().BatchWriteFactsMustExist(ctx, branch, nil, []string{existing}, []string{existing}, "retract", "retract")
				return err
			},
			"batch write + retract": func() error {
				_, _, err := svc.Facts().BatchWriteFacts(ctx, branch, map[string]string{"kb/principles/ui/b.md": good}, []string{existing}, "move", "learn")
				return err
			},
		} {
			err := mutate()
			require.ErrorIs(t, err, facts.ErrOntologyUnusable, "%s must be refused while the ontology is unusable", name)
			require.Contains(t, err.Error(), "no ontology at", name)
		}
		require.Equal(t, before, head(), "nothing moved the branch")

		_, err = svc.Facts().WriteFact(ctx, branch, "notes.txt", "x\n", "a non-fact", "updated")
		require.NoError(t, err, "a write that touches no fact passes the gate")
		_, err = svc.Facts().WriteFact(ctx, branch, fact.OntologyFile, codeOntologyYAML(t)+"\n", "repair", "updated")
		require.NoError(t, err, "the ontology's own repair passes the gate")

		svc.SetOntologyUnusable(nil)
		_, err = svc.Facts().WriteFact(ctx, branch, "kb/principles/ui/new.md", good, "w", "learn")
		require.NoError(t, err)
		_, err = svc.Facts().DeleteFact(ctx, branch, existing, "prune")
		require.NoError(t, err, "with the gate lifted, deletes land again")
	})
}
