package httpx

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type peekReq struct {
	Origin *struct {
		URL      string `json:"url"`
		SecretID string `json:"secret_id"`
	} `json:"origin"`
	SecretID string `json:"secret_id"`
}

// PeekJSON sees what DecodeJSON hands the handler, and leaves the body whole.
func TestPeekJSON_SameDecodeAsTheHandler(t *testing.T) {
	for _, c := range []struct {
		body       string
		ok         bool
		secret     string
		origin     bool
		originSecr string
	}{
		{`{"SECRET_ID":"s"}`, true, "s", false, ""},
		{`{"origin":{"url":"u"},"origin":{"Secret_Id":"s"}}`, true, "", true, "s"},
		{`{"origin":{}}`, true, "", true, ""},
		{``, true, "", false, ""},
		{`   `, true, "", false, ""},
		{`{"a":1} {"secret_id":"s"}`, false, "", false, ""},
		{`{nope`, false, "", false, ""},
		{`[1]`, false, "", false, ""},
	} {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(c.body))
		var v peekReq
		ok := PeekJSON(req, &v, 0)
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.body, ok, c.ok)
			continue
		}
		if ok && (v.SecretID != c.secret || (v.Origin != nil) != c.origin ||
			(v.Origin != nil && v.Origin.SecretID != c.originSecr)) {
			t.Errorf("%q: decoded %+v", c.body, v)
		}
		rest, _ := io.ReadAll(req.Body)
		if string(rest) != c.body {
			t.Errorf("%q: the handler would read %q", c.body, rest)
		}
	}
}

// A body over the limit is refused (the caller decides the strictest way),
// and the handler still gets every byte.
func TestPeekJSON_OverLimit(t *testing.T) {
	body := `{"secret_id":"` + strings.Repeat("x", 100) + `"}`
	req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	var v peekReq
	if PeekJSON(req, &v, 50) {
		t.Fatal("a body over the limit was decoded")
	}
	rest, _ := io.ReadAll(req.Body)
	if string(rest) != body {
		t.Fatalf("the handler would read %d of %d bytes", len(rest), len(body))
	}
}
