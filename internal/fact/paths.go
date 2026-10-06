package fact

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// NormalizePath ensures path has the ontologyRoot prefix and .md suffix,
// and lowercases all path segments after the ontology root to prevent
// case-sensitive duplicates (e.g. "AI" vs "ai").
//
// EXCEPTIONS — two kinds of repo-ROOT path are returned without the prefix:
//
//   - a path whose FIRST segment is dot-prefixed (PrivateRoot/…, .github/…).
//     The fact tools refuse it, and they must refuse it under the name the
//     caller passed, not as kb/.knomit/… — an error naming a path the caller
//     never wrote does not say what was wrong.
//   - a path whose first segment is ArtifactsRoot, case-insensitively
//     (artifacts/<area>/…): agents' working files live BESIDE the ontology
//     root, never inside it. So "artifacts/runs/x" means the repo-root
//     folder, never a kb/artifacts/ topic; a fact under such a topic is still
//     reachable by its full kb/… path.
//
// Such a path is lowercased in full: there is no ontology root to lowercase
// "after", and store.writeFile lowercases the whole path anyway.
func NormalizePath(ontologyRoot, path string) string {
	if strings.HasPrefix(path, ".") || IsUnderArtifactsRoot(path) {
		if !strings.HasSuffix(path, ".md") {
			path = path + ".md"
		}
		return strings.ToLower(path)
	}
	prefix := ontologyRoot + "/"
	if !strings.HasPrefix(path, prefix) {
		path = prefix + path
	}
	if !strings.HasSuffix(path, ".md") {
		path = path + ".md"
	}
	// Lowercase everything after the ontology root prefix.
	if strings.HasPrefix(path, prefix) {
		path = prefix + strings.ToLower(path[len(prefix):])
	}
	return path
}

// BuildFactPath generates a unique fact file path: ontologyRoot/topic/category/<uuid8>.md.
// Topic and category are lowercased to prevent case-sensitive duplicates.
func BuildFactPath(ontologyRoot, topic, category string) string {
	id := uuid.New().String()[:8]
	topic = strings.ToLower(topic)
	category = strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(category, "/"), "/"))
	return fmt.Sprintf("%s/%s/%s/%s.md", ontologyRoot, topic, category, id)
}

// IsFactFilePath reports whether a normalized path is one the fact tools may
// open: a .md file under the ontology root, or an artifact (IsArtifactPath) —
// in both cases with no dot-prefixed segment anywhere. Anything else —
// .knomit/skills/x/SKILL.md, .github/notes.md, README.md, kb/.hidden/x.md, a
// directory — is not.
//
// A private path is never a fact file, whatever root it sits in: PrivateRoot
// is the system, written through git and read by knomit by name, and the fact
// tools do not read it either.
func IsFactFilePath(ontologyRoot, path string) bool {
	if !IsMarkdownPath(path) || IsPrivatePath(path) {
		return false
	}
	return strings.HasPrefix(path, ontologyRoot+"/") || IsArtifactPath(path)
}

// IsMarkdownPath reports whether a path names a markdown file — the file kind
// every fact is stored as.
func IsMarkdownPath(path string) bool {
	return strings.HasSuffix(path, ".md")
}
