package mcp

import (
	"fmt"

	"knomit/internal/fact"
)

// closedSuffix is the one sentence every fact tool gives when it refuses a dot
// path, so an agent meets the same explanation at every door.
var closedSuffix = fmt.Sprintf("a path with a segment beginning with '.' is closed to the fact tools: "+
	"%s/ is the system (ontology, triggers, recipes, skills) and changes only through git, "+
	"and other dot paths are not knowledge; write an agent's working file under %s/<area>/ instead",
	fact.PrivateRoot, fact.ArtifactsRoot)

// closedToFactTools returns the refusal for a normalized path the fact tools
// may neither read nor write — any path with a dot segment, .knomit/ included
// (F25) — or "" when the path is not closed on that ground.
func closedToFactTools(path string) string {
	if !fact.IsPrivatePath(path) {
		return ""
	}
	return path + " is private: " + closedSuffix
}

// notAnArtifactPath is the refusal for a path under artifacts/ that is not a
// valid artifact path (too shallow, an empty or dot segment, or "..").
func notAnArtifactPath(path string) string {
	return fmt.Sprintf("%s is not writable: an explicit path must be under %s/<area>/<name>, "+
		"at least one folder deep, with no empty segment, no segment beginning with '.', and no \"..\"",
		path, fact.ArtifactsRoot)
}
