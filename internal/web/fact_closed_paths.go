package web

import (
	"net/http"
	"net/url"

	"knomit/internal/fact"
	"knomit/internal/web/hal"
)

// writePrivatePathProblem is the 400 every REST fact route gives for a path
// with a dot segment, read or write: such a path is closed to the fact
// endpoints (F25). .knomit/ is the system — ontology, triggers, recipes,
// skills — and changes only through git; every other dot path is machinery,
// not knowledge. knomit itself still reads .knomit/ by name; those reads do
// not go through these routes.
func writePrivatePathProblem(w http.ResponseWriter, r *http.Request, path string) {
	hal.WriteProblem(w, http.StatusBadRequest, "Private path",
		path+" is private: a path with a segment beginning with '.' is closed to the fact endpoints: "+
			fact.PrivateRoot+"/ is the system and changes only through git, and other dot paths are not knowledge; "+
			"an agent's working file belongs under "+fact.ArtifactsRoot+"/<area>/", r.URL.Path)
}

// refusePrivateRead writes the 400 and reports true when a fact READ route is
// asked for a path with a dot segment. It judges the path both as captured
// and percent-decoded, so "%2Eknomit/…" is not a way around it. The
// sub-resource suffixes (/commits, /incoming, /outgoing) carry no dot, so the
// full captured path can be judged before they are split off.
func refusePrivateRead(w http.ResponseWriter, r *http.Request, captured string) bool {
	decoded := captured
	if dec, err := url.PathUnescape(captured); err == nil {
		decoded = dec
	}
	if fact.IsPrivatePath(captured) || fact.IsPrivatePath(decoded) {
		writePrivatePathProblem(w, r, decoded)
		return true
	}
	return false
}
