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
// EXCEPTION: a path whose FIRST segment is dot-prefixed is a repo-ROOT path
// (PrivateRoot/…, .github/…), not an ontology path, and is returned without
// the prefix. Without this, a job writing .knomit/jobs/x.md would silently be
// redirected to kb/.knomit/jobs/x.md — a path IsWritablePrivatePath refuses,
// so the write fails with an error that does not match what the caller passed.
// Such a path is lowercased in full: there is no ontology root to lowercase
// "after", and store.writeFile lowercases the whole path anyway.
func NormalizePath(ontologyRoot, path string) string {
	if strings.HasPrefix(path, ".") {
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
