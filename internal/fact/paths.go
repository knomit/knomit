package fact

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// NormalizePath ensures path has the ontologyRoot prefix and .md suffix.
func NormalizePath(ontologyRoot, path string) string {
	prefix := ontologyRoot + "/"
	if !strings.HasPrefix(path, prefix) {
		path = prefix + path
	}
	if !strings.HasSuffix(path, ".md") {
		path = path + ".md"
	}
	return path
}

// BuildFactPath generates a unique fact file path: ontologyRoot/topic/category/<uuid8>.md.
func BuildFactPath(ontologyRoot, topic, category string) string {
	id := uuid.New().String()[:8]
	category = strings.TrimPrefix(category, "/")
	category = strings.TrimSuffix(category, "/")
	return fmt.Sprintf("%s/%s/%s/%s.md", ontologyRoot, topic, category, id)
}
