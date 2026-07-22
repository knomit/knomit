package repos

import (
	"context"
	"fmt"
)

// LensResolveKind classifies why resolving a {lens} URL segment failed.
//
// Resolution is domain logic — registry lookup and binding construction — and
// lives here so packages that must not import internal/web (internal/mcp) can
// drive it. The HTTP rendering of these failures lives in
// internal/web.LensMiddleware, which maps each kind to a status and a
// problem+json title. Keeping the split means repos never learns about HTTP
// while callers still get a loud, classified failure rather than a bare error
// string they would have to pattern-match.
type LensResolveKind int

const (
	// LensRegistryUnavailable: the manager's lens registry has not started.
	LensRegistryUnavailable LensResolveKind = iota
	// LensRegistryError: the registry was reachable but the lookup failed.
	LensRegistryError
	// LensNotFound: no lens is registered under that name.
	LensNotFound
	// LensUnavailable: the lens exists but one of its member repos is not
	// available, so the binding cannot be built. A lens never silently
	// shrinks its read set (lenses RFC §9.1).
	LensUnavailable
)

// LensResolveError is a classified lens-resolution failure.
type LensResolveError struct {
	Kind LensResolveKind
	Err  error
}

func (e *LensResolveError) Error() string { return e.Err.Error() }
func (e *LensResolveError) Unwrap() error { return e.Err }

// ResolveLensBinding resolves a lens name against the Manager's registry,
// builds its Binding, and returns ctx decorated with both the Binding and the
// lens's write repo as the context RepoInstance (repo-based paths like session
// instructions read the latter).
//
// Resolution fails loudly: every failure returns a *LensResolveError carrying
// the kind, so no caller can mistake an unavailable member for an empty lens.
func ResolveLensBinding(ctx context.Context, m *Manager, name string) (context.Context, error) {
	reg := m.Registry()
	if reg == nil {
		return nil, &LensResolveError{
			Kind: LensRegistryUnavailable,
			Err:  fmt.Errorf("lens registry not started"),
		}
	}
	l, ok, err := reg.Get(name)
	if err != nil {
		return nil, &LensResolveError{
			Kind: LensRegistryError,
			Err:  fmt.Errorf("lens registry: %w", err),
		}
	}
	if !ok {
		return nil, &LensResolveError{
			Kind: LensNotFound,
			Err:  fmt.Errorf("lens not found: %s", name),
		}
	}
	b, err := NewBindingOfLens(m, l)
	if err != nil {
		return nil, &LensResolveError{Kind: LensUnavailable, Err: err}
	}
	ctx = WithBinding(ctx, b)
	ctx = WithRepoInstance(ctx, b.Write())
	return ctx, nil
}
