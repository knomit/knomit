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

	t.Run("gate", func(t *testing.T) {
		svc := fresh(t, codeOntologyYAML(t))
		svc.SetOntologyUnusable(errors.New("no ontology at .knomit/ontology.yaml"))
		_, err := svc.Facts().WriteFact(ctx, branch, "kb/principles/ui/new.md", good, "w", "learn")
		require.ErrorIs(t, err, facts.ErrOntologyUnusable)
		require.Contains(t, err.Error(), "no ontology at")
		_, err = svc.Facts().WriteFact(ctx, branch, "notes.txt", "x\n", "a non-fact", "updated")
		require.NoError(t, err, "a write that touches no fact passes the gate")
		svc.SetOntologyUnusable(nil)
		_, err = svc.Facts().WriteFact(ctx, branch, "kb/principles/ui/new.md", good, "w", "learn")
		require.NoError(t, err)
	})
}
