package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// codeOntologyYAML is the baked code preset, whose principles topic declares
// must-have-designer-entity and three more rules.
func codeOntologyYAML(t *testing.T) string {
	t.Helper()
	out, err := fact.CodeOntology().Serialize()
	require.NoError(t, err)
	return string(out)
}

// principle renders a principles fact; designer adds the entity the topic's
// must-have-designer-entity rule requires.
func principle(t *testing.T, body string, designer bool) string {
	t.Helper()
	f := fact.NewFact("placeholder.md")
	f.Title, f.Body = "A principle", body
	f.Kind, f.Type = fact.Pragmatic, fact.Policy
	f.Confidence, f.Sources, f.Domain = 0.9, 1, []string{"global"}
	if designer {
		f.Entities = []string{"designer"}
	}
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return out
}

const legacyPrinciple = "kb/principles/ui/legacy.md"

// newValidatedStore is a repository on the code preset whose main and agent
// branch hold one LEGACY principle that fails must-have-designer-entity
// (written by the repository's init, where no rule runs: as the 18 legacy
// principles of knomit-kb and agentic-engineering-kb were written).
func newValidatedStore(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{
		fact.OntologyFile: codeOntologyYAML(t),
		legacyPrinciple:   principle(t, "legacy body", false),
	}, "agent/test"))
	return svc
}

// RuleValidation at the source: every fact write that commits through the
// store's two write cores (writeFileExact: WriteFact, WriteFactIfUnchanged;
// batchWriteLocked: BatchWriteFacts and friends) is validated against the
// ontology at the branch's tip before any object is written. A new invalid
// principle is refused with the validation's own message; an EDIT of the
// legacy invalid principle that does not fix it is refused; one that fixes
// it lands; a write that leaves the legacy fact byte-identical, a write of
// another fact beside it, and its delete are not refused (an untouched
// legacy fact is never judged). The row count is asserted.
//
// SABOTAGE: drop the validateWrite call from writeFileExact -> the WriteFact
// rows land -> red; drop it from batchWriteLocked -> the batch rows land ->
// red; validate unchanged content too (drop the old == content skip) -> the
// byte-identical row is refused -> red.
func TestValidateWrite_TheStoreRefusesAnInvalidFactAtTheSource(t *testing.T) {
	ctx := context.Background()
	type write func(svc *Service) error
	single := func(p, c string) write {
		return func(svc *Service) error {
			_, err := svc.Facts().WriteFact(ctx, "agent/test", p, c, "w", "learn")
			return err
		}
	}
	batch := func(p, c string) write {
		return func(svc *Service) error {
			_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", map[string]string{p: c}, nil, "w", "review")
			return err
		}
	}
	rows := []struct {
		name    string
		w       func(t *testing.T) write
		refused bool
	}{
		{"WriteFact: a new principle without designer", func(t *testing.T) write { return single("kb/principles/ux/new.md", principle(t, "new", false)) }, true},
		{"BatchWriteFacts: a new principle without designer", func(t *testing.T) write { return batch("kb/principles/ux/new.md", principle(t, "new", false)) }, true},
		{"WriteFact: the legacy principle edited, not fixed", func(t *testing.T) write { return single(legacyPrinciple, principle(t, "edited body", false)) }, true},
		{"BatchWriteFacts: the legacy principle edited, not fixed", func(t *testing.T) write { return batch(legacyPrinciple, principle(t, "edited body", false)) }, true},
		{"WriteFact: the legacy principle fixed", func(t *testing.T) write { return single(legacyPrinciple, principle(t, "edited body", true)) }, false},
		{"BatchWriteFacts: a valid new principle", func(t *testing.T) write { return batch("kb/principles/ux/new.md", principle(t, "new", true)) }, false},
		{"WriteFact: the legacy principle rewritten byte-identical", func(t *testing.T) write { return single(legacyPrinciple, principle(t, "legacy body", false)) }, false},
		{"WriteFact: another fact beside the untouched legacy one", func(t *testing.T) write {
			return single("kb/decisions/x/y.md", "---\ntype: observation\n---\n# y\n\ny\n")
		}, false},
		{"BatchWriteFacts: the legacy principle deleted", func(t *testing.T) write {
			return func(svc *Service) error {
				_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", nil, []string{legacyPrinciple}, "w", "retract")
				return err
			}
		}, false},
	}
	ran := 0
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			svc := newValidatedStore(t)
			before := svc.rh.resolveRefMust(t, "agent/test")
			err := r.w(t)(svc)
			if r.refused {
				var fe *FactInvalidError
				require.True(t, errors.As(err, &fe), "want a validation refusal, got %v", err)
				require.ErrorIs(t, err, ErrFactInvalid)
				require.Contains(t, err.Error(), "must-have-designer-entity")
				require.Contains(t, err.Error(), "principles must be authored via /knomit-principle")
				require.Equal(t, before, svc.rh.resolveRefMust(t, "agent/test"), "nothing was written")
			} else {
				require.NoError(t, err)
			}
			ran++
		})
	}
	require.Equal(t, len(rows), ran)
}

func (rh *repoHandler) resolveRefMust(t *testing.T, branch string) string {
	t.Helper()
	h, err := rh.resolveRef(context.Background(), branch)
	require.NoError(t, err)
	return h.String()
}
