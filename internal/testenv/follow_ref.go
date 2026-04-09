package testenv

import "strings"

// FollowRef reads `refPath` at THE SAME COMMIT this FactHandle was
// resolved at. This is the critical temporal-graph invariant from
// working.md: if fact A at commit C1 refers to fact B, following that
// ref must return B as it existed at C1 — not B@HEAD.
//
// The returned FactHandle has one of three states:
//
//   - FactStateExists: the ref looks local (ends in .md, no scheme://)
//     AND the target is present at f's commit.
//   - FactStateBroken: the ref looks local but the target is missing at
//     f's commit (deleted in a later commit we aren't reading, or
//     never existed). Use MustBeBroken to assert.
//   - FactStateExternal: the ref is not a local fact path (URL scheme
//     present, or no .md suffix). Use MustBeExternalRef to assert.
//
// FollowRef is one-step. To walk a chain, call FollowRef on the result:
//
//	at := snap.Fact("kb/a.md").FollowRef("kb/b.md").FollowRef("kb/c.md")
//
// Never returns nil — even Broken/External cases return a non-nil
// handle so tests can branch on state without nil-checking. The
// receiver must be in FactStateExists (enforced by MustExist at entry).
func (f *FactHandle) FollowRef(refPath string) *FactHandle {
	f.MustExist()
	t := f.t
	t.Helper()

	if !looksLikeLocalRef(refPath) {
		return &FactHandle{
			t:      t,
			branch: f.branch,
			commit: f.commit,
			path:   refPath,
			state:  FactStateExternal,
		}
	}

	resolved := resolveFactAtCommit(t, f.branch, f.commit, refPath)
	if resolved.state == FactStateMissing {
		return &FactHandle{
			t:      t,
			branch: f.branch,
			commit: f.commit,
			path:   refPath,
			state:  FactStateBroken,
		}
	}
	return resolved
}

// looksLikeLocalRef reports whether refPath is a candidate for a local
// fact: no URL scheme and ends in ".md".
//
// This mirrors the production distinction in synthesize/decision.go:156
// which checks strings.HasSuffix(r, ".md") to tell local refs from
// external URLs.
func looksLikeLocalRef(refPath string) bool {
	if strings.Contains(refPath, "://") {
		return false
	}
	return strings.HasSuffix(refPath, ".md")
}
