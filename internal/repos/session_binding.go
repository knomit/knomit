package repos

import (
	"context"
	"fmt"
	"strings"
)

// SessionBindingKind classifies why a STORED session binding could not be
// turned back into a live Binding.
//
// It mirrors LensResolveKind's split of concerns: resolution is domain logic
// and lives here, while the caller decides how to render it. The difference is
// where the failure surfaces — a lens URL that will not resolve is an HTTP
// status, but a stored pin that will not resolve must stay inside the JSON-RPC
// envelope as a tool error, because the agent is the only party that can fix
// it (by binding again).
type SessionBindingKind int

const (
	// BindingMalformed: the stored value is not `repo:<uid>` / `lens:<uid>`.
	BindingMalformed SessionBindingKind = iota
	// BindingRepoUnavailable: no live instance for that repo uid.
	BindingRepoUnavailable
	// BindingLensUnavailable: the lens is gone, or a member repo is.
	BindingLensUnavailable
)

// SessionBindingError is a classified stored-binding resolution failure. Its
// message is read by an LLM, so every kind names the thing that failed and
// ends by saying what to do about it.
type SessionBindingError struct {
	Kind SessionBindingKind
	Err  error
}

func (e *SessionBindingError) Error() string { return e.Err.Error() }
func (e *SessionBindingError) Unwrap() error { return e.Err }

// ParsePin splits a stored pin into its kind and uid. Anything that is not
// exactly `repo:<uid>` or `lens:<uid>` with a non-empty uid is an error: the
// pin is machine-written, so a malformed one is corruption, not user input to
// be salvaged.
func ParsePin(pin string) (kind, uid string, err error) {
	k, u, ok := strings.Cut(pin, ":")
	if !ok || u == "" || (k != "repo" && k != "lens") {
		return "", "", fmt.Errorf("malformed binding pin %q", pin)
	}
	return k, u, nil
}

// PinForRepo and PinForLens build the stored value from a live object. They
// exist so the pin format is spelled in ONE place next to pinOf — a session
// binding and a tool cursor must agree on it byte for byte.
func PinForRepo(ri *RepoInstance) string { return pinOf("repo:", ri.UID()) }

// PinForLens is PinForRepo's counterpart for a lens.
func PinForLens(l Lens) string { return pinOf("lens:", l.UID) }

// ResolveSessionBinding turns a stored pin into a context carrying the Binding
// and the write repo as the context RepoInstance — the same two values
// ResolveLensBinding sets, so everything downstream of either mount reads an
// identically shaped context.
//
// Write eligibility needs no logic here: NewBindingOfRepo(ri, "") binds at the
// repo's own read branch (the agent branch, or the followed upstream for a
// subscription) and takes writeOK from WritableBranch, which is false for a
// subscription. Every write tool already gates on Binding.WriteOK().
func ResolveSessionBinding(ctx context.Context, m *Manager, pin string) (context.Context, error) {
	kind, uid, err := ParsePin(pin)
	if err != nil {
		return nil, &SessionBindingError{
			Kind: BindingMalformed,
			Err:  fmt.Errorf("stored binding %q is malformed — call knomit_bind again", pin),
		}
	}

	var b *Binding
	switch kind {
	case "repo":
		ri := m.GetByUID(uid)
		if ri == nil {
			return nil, &SessionBindingError{
				Kind: BindingRepoUnavailable,
				Err: fmt.Errorf("bound repo %q is not available (deleted, archived or failed to open) — call knomit_bind again",
					m.repoLabel(uid)),
			}
		}
		b = NewBindingOfRepo(ri, "")
	case "lens":
		reg := m.LensRegistry()
		if reg == nil {
			return nil, &SessionBindingError{
				Kind: BindingLensUnavailable,
				Err:  fmt.Errorf("lens registry not started — call knomit_bind again"),
			}
		}
		l, ok, lerr := reg.GetByUID(uid)
		if lerr != nil || !ok {
			return nil, &SessionBindingError{
				Kind: BindingLensUnavailable,
				Err:  fmt.Errorf("bound lens %q is not available — call knomit_bind again", uid),
			}
		}
		lb, berr := NewBindingOfLens(m, l)
		if berr != nil {
			return nil, &SessionBindingError{
				Kind: BindingLensUnavailable,
				Err:  fmt.Errorf("%w — call knomit_bind again", berr),
			}
		}
		b = lb
	}

	ctx = WithBinding(ctx, b)
	ctx = WithRepoInstance(ctx, b.Write())
	return ctx, nil
}
