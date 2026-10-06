package fact

import "strings"

// ArtifactsRoot is the repo-root folder that holds agents' untyped working
// files: a job's bookkeeping, a run's notes — anything an agent keeps that is
// not knowledge.
//
// A knomit repo holds three kinds of data, one root each (F25, user ruling
// 2026-10-05):
//
//   - the ontology root (kb/): facts, read and written by agents through the
//     fact tools by topic and category;
//   - PrivateRoot (.knomit/): the SYSTEM — ontology, triggers, recipes,
//     skills — written by people through git and by knomit's own code, and
//     closed to every fact tool together with every other dot path;
//   - ArtifactsRoot (artifacts/): agents' untyped files, written and read by
//     the fact tools' explicit `path` argument only.
//
// Location is the whole filter. The index, the OKF export, knomit_changes and
// the trigger diff all key on the ontology root, so a folder beside it is
// outside all four with no filter of its own: never indexed, never returned
// by knomit_query, never exported, never seen by a trigger. That only holds
// while the ontology root is not this folder, which is why config refuses an
// ontology_root of "artifacts" (or one nested under it).
const ArtifactsRoot = "artifacts"

// IsArtifactPath reports whether path is a writable artifact path:
// ArtifactsRoot/<area>/<name…>, at least one subdirectory deep, with no
// segment that is empty, ".", or begins with "." — at ANY depth — and no ".."
// anywhere.
//
// The path is judged AS GIVEN and case-sensitively: callers pass a normalized
// (lowercased) path. A caller holding a verbatim path (the REST fact routes)
// lowercases it first, because the store lowercases the whole path when it
// writes and "Artifacts/x.md" would otherwise land as "artifacts/x.md" without
// ever having been judged.
//
// This is an AUTHORIZATION predicate, so it must be self-sufficient: ".."
// is rejected as a substring because that is the rule store.validatePath
// enforces (a predicate that authorizes what the writer refuses takes a whole
// all-or-nothing learn batch down with the wrong error), and "." and empty
// segments are rejected rather than trusted to be normalized away.
//
// No dot segment at any depth, unlike the pre-F25 .knomit/<area>/ rule, which
// checked only <area>: a dot path is private (IsPrivatePath) and the fact
// tools never write one, inside this root or out of it.
func IsArtifactPath(path string) bool {
	rest, ok := strings.CutPrefix(path, ArtifactsRoot+"/")
	if !ok {
		return false
	}
	if strings.Contains(rest, "..") {
		return false
	}
	segs := strings.Split(rest, "/")
	// At least one subdirectory deep: <area>/<name>.
	if len(segs) < 2 {
		return false
	}
	for _, seg := range segs {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// IsUnderArtifactsRoot reports whether path names ArtifactsRoot or anything
// under it, case-insensitively. It is the "does this caller MEAN the artifacts
// folder" test, used where the path arrives verbatim: such a path must then
// pass IsArtifactPath on its lowercased form, or be refused.
func IsUnderArtifactsRoot(path string) bool {
	lower := strings.ToLower(path)
	return lower == ArtifactsRoot || strings.HasPrefix(lower, ArtifactsRoot+"/")
}
