package hal

import (
	"encoding/json"
	"net/http"
)

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
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}
