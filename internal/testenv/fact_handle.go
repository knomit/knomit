package testenv

import (
	"context"
	"errors"
	"strings"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// FactState classifies the result of resolving a fact at (branch, commit).
//
// The four states exist because the DSL needs to express not just "fact
// present / not present" but also the specialised cases that FollowRef
// (Task 2.8) can produce:
//
//   - Broken: the parent fact references a local path that is absent at
//     the requested commit (e.g. the target was deleted in a later commit
//     that we aren't looking at, but from this commit's perspective it's
//     just gone).
//   - External: the ref does not look like a local fact path at all
//     (e.g. http://…) so it is outside the graph by design.
//
// Field assertion methods (Title, Confidence, Refs, etc.) all require
// State == FactStateExists. Missing / Broken / External FactHandles trip
// t.Fatalf on field access — use the MustBe* methods to assert state
// explicitly first.
type FactState int

const (
	FactStateExists FactState = iota
	FactStateMissing
	FactStateBroken
	FactStateExternal
)

// String returns a human-readable name for the state. Used in error
// messages from the Must* methods.
func (s FactState) String() string {
	switch s {
	case FactStateExists:
		return "Exists"
	case FactStateMissing:
		return "Missing"
	case FactStateBroken:
		return "Broken"
	case FactStateExternal:
		return "External"
	default:
		return "Unknown"
	}
}

// FactHandle represents a (branch, commit, path) tuple plus the resolved
// state of the fact at that position. Created by Snapshot.Fact (Task 2.7)
// or by FactHandle.FollowRef (Task 2.8).
//
// Field assertions (Title, Confidence, Refs, etc.) implicitly require
// State == FactStateExists and will fail the test otherwise. Use
// MustExist / MustNotExist / MustBeBroken / MustBeExternalRef to assert
// state explicitly.
type FactHandle struct {
	t       *testing.T
	branch  *BranchHandle
	commit  string
	path    string
	state   FactState
	parsed  *fact.Fact
	rawText string
}

// State returns the resolved state of the fact at this (branch, commit, path).
func (f *FactHandle) State() FactState { return f.state }

// Path returns the path this handle was resolved for.
func (f *FactHandle) Path() string { return f.path }

// Commit returns the commit hash this handle was resolved at.
func (f *FactHandle) Commit() string { return f.commit }

// MustExist asserts the fact is present at this commit and parses cleanly.
// Returns the handle for chaining.
func (f *FactHandle) MustExist() *FactHandle {
	f.t.Helper()
	if f.state != FactStateExists {
		f.t.Fatalf("FactHandle(%s @ %s): expected Exists, got %v", f.path, f.commit, f.state)
	}
	return f
}

// MustNotExist asserts the fact is not present at this commit.
func (f *FactHandle) MustNotExist() {
	f.t.Helper()
	if f.state != FactStateMissing {
		f.t.Fatalf("FactHandle(%s @ %s): expected Missing, got %v", f.path, f.commit, f.state)
	}
}

// MustBeBroken asserts FollowRef returned a broken-ref handle: the ref
// looked local (ended in .md) but the target was missing at the commit.
func (f *FactHandle) MustBeBroken() {
	f.t.Helper()
	if f.state != FactStateBroken {
		f.t.Fatalf("FactHandle(%s @ %s): expected Broken, got %v", f.path, f.commit, f.state)
	}
}

// MustBeExternalRef asserts FollowRef returned an external-ref handle:
// the ref did not look like a local fact path (scheme:// or no .md suffix).
func (f *FactHandle) MustBeExternalRef() {
	f.t.Helper()
	if f.state != FactStateExternal {
		f.t.Fatalf("FactHandle(%s @ %s): expected External, got %v", f.path, f.commit, f.state)
	}
}

// Raw returns the parsed Fact struct. Requires State == Exists.
func (f *FactHandle) Raw() fact.Fact {
	f.MustExist()
	return *f.parsed
}

// RawContent returns the raw fact file text (YAML frontmatter + body).
// Requires State == Exists.
func (f *FactHandle) RawContent() string {
	f.MustExist()
	return f.rawText
}

// Title returns a StringAssert over the fact's title.
func (f *FactHandle) Title() *StringAssert {
	f.MustExist()
	return &StringAssert{t: f.t, got: f.parsed.Title, label: f.path + ".title"}
}

// Body returns a StringAssert over the fact's body text (after the title heading).
func (f *FactHandle) Body() *StringAssert {
	f.MustExist()
	return &StringAssert{t: f.t, got: f.parsed.Body, label: f.path + ".body"}
}

// Type returns a StringAssert over the fact's epistemic type.
func (f *FactHandle) Type() *StringAssert {
	f.MustExist()
	return &StringAssert{t: f.t, got: string(f.parsed.Type), label: f.path + ".type"}
}

// Confidence returns a FloatAssert over the fact's confidence score.
func (f *FactHandle) Confidence() *FloatAssert {
	f.MustExist()
	return &FloatAssert{t: f.t, got: f.parsed.Confidence, label: f.path + ".confidence"}
}

// Domain returns a StringSliceAssert over the fact's domain list.
func (f *FactHandle) Domain() *StringSliceAssert {
	f.MustExist()
	return &StringSliceAssert{t: f.t, got: f.parsed.Domain, label: f.path + ".domain"}
}

// Entities returns a StringSliceAssert over the fact's entity list.
func (f *FactHandle) Entities() *StringSliceAssert {
	f.MustExist()
	return &StringSliceAssert{t: f.t, got: f.parsed.Entities, label: f.path + ".entities"}
}

// Refs returns a StringSliceAssert over the fact's ref list.
func (f *FactHandle) Refs() *StringSliceAssert {
	f.MustExist()
	return &StringSliceAssert{t: f.t, got: f.parsed.Refs, label: f.path + ".refs"}
}

// Sources returns an IntAssert over the fact's source count.
func (f *FactHandle) Sources() *IntAssert {
	f.MustExist()
	return &IntAssert{t: f.t, got: f.parsed.Sources, label: f.path + ".sources"}
}

// resolveFactAtCommit reads `path` from `branch` at `commit` and returns a
// FactHandle in the appropriate state. This is the canonical construction
// path used by Snapshot.Fact and FactHandle.FollowRef (Task 2.7/2.8).
//
// The function never returns nil. Callers branch on State().
func resolveFactAtCommit(t *testing.T, b *BranchHandle, commit, path string) *FactHandle {
	t.Helper()
	fh := &FactHandle{t: t, branch: b, commit: commit, path: path}

	var res store.ReadFactResult
	var readErr error
	b.repo.ri.WithRead(func(svc *store.Service) {
		res, readErr = svc.Facts().ReadFact(
			context.Background(), b.name, path, &store.ReadFactOpts{AtCommit: commit})
	})
	if readErr != nil {
		if isNotFoundErr(readErr) {
			fh.state = FactStateMissing
			return fh
		}
		t.Fatalf("ReadFact(%s @ %s): %v", path, commit, readErr)
	}
	if res.Content == "" {
		fh.state = FactStateMissing
		return fh
	}
	parsed, err := fact.ParseFact(path, res.Content)
	if err != nil {
		// Raw content is there but doesn't parse. This is distinct from
		// Missing — the tree has the file but it's malformed. Treat as
		// Broken for handle purposes; the scenario test will either assert
		// MustBeBroken or escalate as a production divergence.
		fh.state = FactStateBroken
		fh.rawText = res.Content
		return fh
	}
	fh.parsed = &parsed
	fh.rawText = res.Content
	fh.state = FactStateExists
	return fh
}

// isNotFoundErr reports whether err is a "fact not found at this commit"
// signal from the store. The store currently returns wrapped errors whose
// text contains "not found" / "ErrEntryNotFound" / "directory not found"
// for missing tree entries — we match heuristically because there is no
// typed sentinel exposed today.
//
// TODO(testenv): replace with an errors.Is check once store exposes a
// typed ErrFactNotFound.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "not found") || strings.Contains(msg, "ErrEntryNotFound") || strings.Contains(msg, "directory not found") {
		return true
	}
	// Unwrap chain — future typed errors will land here.
	var stubSentinel = errors.New("placeholder")
	_ = stubSentinel
	return false
}
