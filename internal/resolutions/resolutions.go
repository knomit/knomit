// Package resolutions normalizes the {body} resolutions that settle a refused
// experiment commit.
//
// It exists as its own package for ONE reason: the MCP tool and the REST
// endpoint are two doors onto the same action, and a body accepted at one and
// refused at the other would mean the two disagree about what `commit` does.
// Both call Normalize; neither validates anything itself.
//
// WHAT A {body} RESOLUTION ACTUALLY IS: a fact write. It lands content on a
// branch under a path, exactly as knomit_update does — so it goes through the
// same chain, in the same order, and what gets committed is what SerializeFact
// produces, never the caller's raw bytes:
//
//	ParseFact  →  ValidateFact  →  refs.Gate.Apply  →  SerializeFact
//
// The subject-motif gate is NOT a step here, deliberately: SerializeFact
// applies it internally, so reaching SerializeFact IS being gated. The REST
// raw-editor PUT invokes that gate by hand only because it commits the
// client's bytes and never reaches SerializeFact at all — copying its call
// into this path was surplus. internal/fact's MN4 conformance test enforces
// exactly that, and enforces it by SCANNING SOURCE TEXT: naming the helper
// here at all, even in a comment, trips it.
//
// Skipping any link makes a resolution the one way into the corpus that
// bypasses a rule every other write path enforces. Two of those links are not
// optional in practice and were shipped broken before this package existed:
// ParseFact is the LENIENT read side and accepts things SerializeFact rejects
// (a malformed `kb://` or `src://` ref passes the parse and dies on the way
// out), and the refs gate is documented to cover EVERY write path
// (kb/decisions/mcp/learn-rejects-unresolvable-local-refs).
package resolutions

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	knomitfact "knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Normalize validates every {body} resolution and rewrites it to the bytes
// that should actually be committed. Side-only resolutions pass through
// untouched — they name an existing version rather than introducing content.
//
// `parent` is the branch the commit lands on and `experiment` the branch it
// comes from. A local ref is accepted when it resolves on EITHER: the merged
// tree contains both sides' facts, so checking only the parent would refuse a
// body citing a fact this very merge is bringing in.
func Normalize(
	ctx context.Context,
	ri *repos.RepoInstance,
	parent, experiment string,
	in map[string]store.Resolution,
) (map[string]store.Resolution, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]store.Resolution, len(in))
	for file, res := range in {
		if res.Body == nil {
			out[file] = res
			continue
		}
		normalized, err := normalizeOne(ctx, ri, parent, experiment, file, res.Body)
		if err != nil {
			return nil, err
		}
		out[file] = store.Resolution{Body: normalized}
	}
	return out, nil
}

func normalizeOne(
	ctx context.Context,
	ri *repos.RepoInstance,
	parent, experiment, file string,
	body []byte,
) ([]byte, error) {
	// 1. Parse. Catches prose with no frontmatter, and an empty title heading —
	// the shape gotchas/mcp/learn/empty-title-unvalidated records as invisible
	// to query, unreadable by explain, and repairable only by retract+relearn.
	f, err := knomitfact.ParseFact(file, string(body))
	if err != nil {
		return nil, fmt.Errorf("resolution body for %q is not a valid fact: %w", file, err)
	}

	// 2. The ontology's own rules, judged at the topic the PATH places the fact
	// in — the same derivation knomit_update uses. Private state is skipped
	// wholesale, as every write path skips it: a .knomit/<area>/ path has no
	// ontology placement and ValidateFact runs the ROOT rules unconditionally.
	if ontology := ri.Ontology(); ontology != nil && !knomitfact.IsWritablePrivatePath(file) {
		topicCategory := path.Dir(strings.TrimPrefix(file, ri.OntologyRoot()+"/"))
		if err := knomitfact.ValidateFact(ontology, topicCategory, f); err != nil {
			return nil, fmt.Errorf("resolution body for %q: %w", file, err)
		}
	}

	// 3. The refs gate. Every ref in the body is treated as newly added: a
	// resolution replaces the fact wholesale and the caller composed this text
	// just now, so there is no earlier version whose citations it inherited
	// without choosing them.
	gate, err := resolveGate(ctx, ri, parent, experiment)
	if err != nil {
		return nil, err
	}
	canonRefs, _, err := gate.Apply(ctx, file, f.Refs, nil)
	if err != nil {
		return nil, fmt.Errorf("resolution body for %q has unresolvable references: %w", file, err)
	}
	f.Refs = canonRefs

	// 4. Serialize, and LAND THOSE BYTES — not the caller's raw body.
	//
	// This is a deliberate choice, and it is the same one knomit_update makes:
	// the committed content is SerializeFact's output. A raw body that differs
	// from its own serialization (a canonicalized ref, a stripped motif,
	// reordered frontmatter) would otherwise enter the tree in a form that
	// never passed the gate, and every later read would see bytes no write
	// path would have produced.
	//
	// SerializeFact is also the STRICT side of the pair: it rejects the
	// malformed `kb://` and `src://` refs that ParseFact — the lenient read
	// side — waves through. Committing raw bytes would skip it entirely.
	serialized, err := knomitfact.SerializeFact(f)
	if err != nil {
		return nil, fmt.Errorf("resolution body for %q could not be serialized: %w", file, err)
	}
	return []byte(serialized), nil
}

// resolveGate builds the ref gate for a resolution: a local fact resolves if it
// exists on either side of the merge.
func resolveGate(ctx context.Context, ri *repos.RepoInstance, parent, experiment string) (refs.Gate, error) {
	return refs.New(knomitfact.ID12(ri.ID()), func(ctx context.Context, p string) (bool, error) {
		var (
			found bool
			err   error
		)
		ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				return
			}
			for _, branch := range slices.Compact([]string{experiment, parent}) {
				if branch == "" {
					continue
				}
				ok, qerr := svc.FactQuery().FactExistsAt(ctx, branch, p, "")
				if qerr != nil {
					err = qerr
					return
				}
				if ok {
					found = true
					return
				}
			}
		})
		return found, err
	}), nil
}
