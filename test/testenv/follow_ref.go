package testenv

import "knomit/internal/fact"

// FollowRef reads `refPath` at THE SAME COMMIT this FactHandle was
// resolved at. This is the critical temporal-graph invariant from
// working.md: if fact A at commit C1 refers to fact B, following that
// ref must return B as it existed at C1 — not B@HEAD.
//
// The returned FactHandle has one of three states:
//
//   - FactStateExists: fact.ClassifyRef calls the ref a fact in THIS repo
//     AND the target is present at f's commit.
//   - FactStateBroken: it names a fact in this repo but the target is
//     missing at f's commit (deleted in a later commit we aren't reading,
//     or never existed). Use MustBeBroken to assert.
//   - FactStateExternal: any other kind — a source citation, another
//     repo's fact, or a URL. Use MustBeExternalRef to assert.
//
// Classification is fact.ClassifyRef, the single authority, rather than a
// storyboard-local copy of the rule: the copy that used to live here ("no
// scheme and ends in .md") counted a markdown SOURCE citation such as
// src://knomit/.claude/plans/x.md@c as a local fact.
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

	ref := fact.ClassifyRef(refPath, fact.ID12(f.branch.repo.ri.ID()))
	if ref.Kind != fact.RefLocalFact {
		return &FactHandle{
			t:      t,
			branch: f.branch,
			commit: f.commit,
			path:   refPath,
			state:  FactStateExternal,
		}
	}

	// Walk by the repo-relative path, so a ref written in canonical
	// kb://<own-id>/… form resolves exactly like its bare equivalent.
	resolved := resolveFactAtCommit(t, f.branch, f.commit, ref.Path)
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
