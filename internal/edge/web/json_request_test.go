package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodeProblem returns the title and detail of the problem document a failed
// request wrote, or "" and "" when nothing was written. httpx's own tests keep
// a copy: the helper is test-only and the two packages' tests cannot share it.
func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) (title, detail string) {
	t.Helper()
	if rec.Body.Len() == 0 {
		return "", ""
	}
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("response is not a problem document: %v: %s", err, rec.Body.String())
	}
	return p.Title, p.Detail
}

// newJSONRequest is httptest.NewRequest with the Content-Type every JSON API
// body is sent with. Fixtures that post a body to a handler reading it with
// httpx.DecodeJSON build their request here.
func newJSONRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Content-Type", "application/json")
	return r
}
