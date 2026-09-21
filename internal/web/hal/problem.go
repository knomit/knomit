package hal

import (
	"encoding/json"
	"net/http"
)

// ProblemContentType is the media type for RFC 9457 problem responses.
//
// It is a constant so that the compression allowlist in internal/web can
// reference the same string the writer sets: chi's compressor matches the
// media type exactly, so a one-character drift between the two silently
// turns compression off with every test still green.
const ProblemContentType = "application/problem+json"

// Problem is an RFC 9457 problem document.
//
// Type defaults to "about:blank" (RFC 9457 §4.2.1) when no more specific
// problem type URI applies. Status matches the HTTP status code of the
// response. Instance is the request URI that failed.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// WriteProblem writes an application/problem+json response with the given
// HTTP status, title (required), and optional detail + instance fields.
// Type is always "about:blank".
func WriteProblem(w http.ResponseWriter, status int, title, detail, instance string) {
	p := Problem{
		Type:     "about:blank",
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: instance,
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteProblemWithExtra is WriteProblem plus RFC 9457 EXTENSION MEMBERS —
// additional top-level keys alongside type/title/status/detail/instance,
// which §3.2 explicitly allows.
//
// It exists for failures whose repair depends on DATA, not only on prose. A
// refused experiment commit is the case it was added for: the client's next
// move is sync or rollback, and which one is chosen by looking at WHICH paths
// conflicted. Putting that list in the detail string would make every
// consumer parse English.
//
// Extension keys that collide with a standard member are DROPPED rather than
// allowed to overwrite it: a caller that shadowed "status" with a different
// value would produce a document whose two statuses disagree, and silently
// winning is worse than silently missing.
func WriteProblemWithExtra(w http.ResponseWriter, status int, title, detail, instance string, extra map[string]any) {
	doc := map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": status,
	}
	if detail != "" {
		doc["detail"] = detail
	}
	if instance != "" {
		doc["instance"] = instance
	}
	for k, v := range extra {
		switch k {
		case "type", "title", "status", "detail", "instance":
			continue
		}
		doc[k] = v
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}
