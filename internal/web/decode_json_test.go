package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readCounter records whether anything read the body, so a 415 can be shown
// to have refused the request without consuming it.
type readCounter struct {
	r    io.Reader
	read bool
}

func (c *readCounter) Read(p []byte) (int, error) {
	c.read = true
	return c.r.Read(p)
}

type decodeTarget struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// decodeProblem returns the title of the problem document a failed decode
// wrote, or "" when nothing was written.
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

func TestDecodeJSON(t *testing.T) {
	const good = `{"name":"a","count":2}`
	cases := []struct {
		name        string
		contentType string // "" = header absent
		body        string
		chunked     bool // ContentLength -1, as a chunked request arrives
		maxBytes    int64
		optional    bool

		wantOK     bool
		wantStatus int    // checked when !wantOK
		wantTitle  string // checked when !wantOK
		wantDetail string // substring, checked when set
		wantUnread bool   // the body must not have been read at all
		want       decodeTarget
	}{
		// ── Content-Type ──
		{name: "absent content type", body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type", wantDetail: "application/json", wantUnread: true},
		{name: "text/plain", contentType: "text/plain", body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type", wantDetail: `"text/plain"`, wantUnread: true},
		{name: "form", contentType: "application/x-www-form-urlencoded", body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type", wantUnread: true},
		{name: "multipart", contentType: "multipart/form-data; boundary=x", body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type", wantUnread: true},
		{name: "json suffix type is not json", contentType: "application/merge-patch+json", body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type", wantUnread: true},
		{name: "unparseable parameter", contentType: "application/json; charset", body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type", wantUnread: true},
		{name: "application/json", contentType: "application/json", body: good,
			wantOK: true, want: decodeTarget{Name: "a", Count: 2}},
		{name: "charset parameter", contentType: "application/json; charset=utf-8", body: good,
			wantOK: true, want: decodeTarget{Name: "a", Count: 2}},
		{name: "upper case", contentType: "APPLICATION/JSON", body: good,
			wantOK: true, want: decodeTarget{Name: "a", Count: 2}},
		{name: "unknown fields are accepted", contentType: "application/json", body: `{"name":"a","extra":true}`,
			wantOK: true, want: decodeTarget{Name: "a"}},

		// ── size ──
		{name: "over the limit", contentType: "application/json", body: `{"name":"` + strings.Repeat("x", 64) + `"}`, maxBytes: 16,
			wantStatus: http.StatusRequestEntityTooLarge, wantTitle: "Request body too large", wantDetail: "16 bytes"},
		{name: "no limit when zero", contentType: "application/json", body: `{"name":"` + strings.Repeat("x", 1<<16) + `"}`,
			wantOK: true, want: decodeTarget{Name: strings.Repeat("x", 1<<16)}},

		// ── malformed ──
		{name: "syntax error names the offset", contentType: "application/json", body: `{"name":"a",}`,
			wantStatus: http.StatusBadRequest, wantTitle: "Invalid request body", wantDetail: "byte 13"},
		{name: "type error names the field and offset", contentType: "application/json", body: `{"name":"a","count":"two"}`,
			wantStatus: http.StatusBadRequest, wantTitle: "Invalid request body", wantDetail: `"count"`},
		{name: "empty body is required", contentType: "application/json", body: "",
			wantStatus: http.StatusBadRequest, wantTitle: "Invalid request body", wantDetail: "empty"},
		{name: "truncated body", contentType: "application/json", body: `{"name":`,
			wantStatus: http.StatusBadRequest, wantTitle: "Invalid request body"},

		// ── optional body ──
		{name: "optional: empty body, no content type", optional: true, body: "",
			wantOK: true},
		{name: "optional: empty chunked body, no content type", optional: true, body: "", chunked: true,
			wantOK: true},
		{name: "optional: body without content type", optional: true, body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type"},
		{name: "optional: chunked body without content type", optional: true, body: good, chunked: true,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type"},
		{name: "optional: chunked json body keeps its first byte", optional: true, contentType: "application/json", body: good, chunked: true,
			wantOK: true, want: decodeTarget{Name: "a", Count: 2}},
		{name: "optional: text/plain body", optional: true, contentType: "text/plain", body: good,
			wantStatus: http.StatusUnsupportedMediaType, wantTitle: "Unsupported Media Type"},
		{name: "optional: malformed json", optional: true, contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest, wantTitle: "Invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &readCounter{r: strings.NewReader(tc.body)}
			r := httptest.NewRequest(http.MethodPost, "/x", body)
			r.ContentLength = int64(len(tc.body))
			if tc.chunked {
				r.ContentLength = -1
			}
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()

			// Pre-filled, so a failed decode that half-wrote v would show.
			v := decodeTarget{Name: "untouched", Count: -1}
			var ok bool
			if tc.optional {
				ok = decodeOptionalJSON(rec, r, &v, tc.maxBytes)
			} else {
				ok = decodeJSON(rec, r, &v, tc.maxBytes)
			}

			title, detail := decodeProblem(t, rec)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status %d, %q: %q)", ok, tc.wantOK, rec.Code, title, detail)
			}
			if tc.wantOK {
				if rec.Body.Len() != 0 {
					t.Fatalf("a successful decode wrote a response: %s", rec.Body.String())
				}
				want := tc.want
				if tc.optional && tc.body == "" {
					want = decodeTarget{Name: "untouched", Count: -1}
				}
				if v != want {
					t.Fatalf("decoded %+v, want %+v", v, want)
				}
				return
			}
			if rec.Code != tc.wantStatus || title != tc.wantTitle {
				t.Fatalf("got %d %q, want %d %q (detail %q)", rec.Code, title, tc.wantStatus, tc.wantTitle, detail)
			}
			if tc.wantDetail != "" && !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("detail %q does not mention %q", detail, tc.wantDetail)
			}
			if tc.wantUnread && body.read {
				t.Fatal("the body was read before the request was refused")
			}
			if v != (decodeTarget{Name: "untouched", Count: -1}) {
				t.Fatalf("a failed decode changed v to %+v", v)
			}
		})
	}
}

// A nil Body (http.NewRequest with no body, as some callers construct it) is an
// empty body to the optional decoder, not a crash.
func TestDecodeOptionalJSON_NilBody(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/x", nil)
	r.Body = nil
	var v decodeTarget
	if !decodeOptionalJSON(httptest.NewRecorder(), r, &v, 0) {
		t.Fatal("a nil body was refused")
	}
}

// newJSONRequest is httptest.NewRequest with the Content-Type every JSON API
// body is sent with. Fixtures that post a body to a handler reading it with
// decodeJSON build their request here; a test about the Content-Type rule
// itself sets or deletes the header afterwards.
func newJSONRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Content-Type", "application/json")
	return r
}
