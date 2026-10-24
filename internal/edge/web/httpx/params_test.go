package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSelfWithQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x?a=1&b=2", nil)
	if got := SelfWithQuery("/base", r); got != "/base?a=1&b=2" {
		t.Errorf("got %q", got)
	}
	bare := httptest.NewRequest(http.MethodGet, "/x", nil)
	if got := SelfWithQuery("/base", bare); got != "/base" {
		t.Errorf("got %q", got)
	}
}
