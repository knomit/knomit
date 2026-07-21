package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLimitParam(t *testing.T) {
	cases := []struct {
		query string
		want  int
		wasOK bool
	}{
		{"", defaultLimit, true},
		{"limit=37", 37, true},
		{"limit=1", 1, true},
		{"limit=500", 500, true},
		{"limit=abc", 0, false},
		{"limit=0", 0, false},
		// A negative limit used to reach the store, where `limit <= 0` clamps
		// to 100 — so this returned MORE rows than the default.
		{"limit=-5", 0, false},
		// Above the ceiling is a 400, not a silent cap: a client that asked
		// for 501 and received 500 cannot distinguish that from exhaustion.
		{"limit=501", 0, false},
		{"limit=1.5", 0, false},
		// Surrounding whitespace is not silently trimmed — the old
		// digit-loop parser rejected it too, but returned 0 (→ default 50)
		// instead of surfacing the bad request.
		{"limit=%205", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)

			got, ok := limitParam(rec, r)
			if ok != tc.wasOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wasOK)
			}
			if !ok {
				if rec.Code != http.StatusBadRequest {
					t.Errorf("status: got %d, want 400", rec.Code)
				}
				if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
					t.Errorf("content-type: got %q", ct)
				}
				return
			}
			if got != tc.want {
				t.Errorf("limit: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOffsetParam(t *testing.T) {
	cases := []struct {
		query string
		want  int
		wasOK bool
	}{
		{"", 0, true},
		{"offset=0", 0, true},
		{"offset=120", 120, true},
		{"offset=abc", 0, false},
		{"offset=-1", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)

			got, ok := offsetParam(rec, r)
			if ok != tc.wasOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wasOK)
			}
			if !ok {
				if rec.Code != http.StatusBadRequest {
					t.Errorf("status: got %d, want 400", rec.Code)
				}
				return
			}
			if got != tc.want {
				t.Errorf("offset: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b", []string{"a", "b"}},
		{",", nil},
		{"  ", nil},
	}

	for _, tc := range cases {
		got := splitCSV(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestSelfWithQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x?a=1&b=2", nil)
	if got := selfWithQuery("/base", r); got != "/base?a=1&b=2" {
		t.Errorf("got %q", got)
	}
	bare := httptest.NewRequest(http.MethodGet, "/x", nil)
	if got := selfWithQuery("/base", bare); got != "/base" {
		t.Errorf("got %q", got)
	}
}
