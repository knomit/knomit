package git

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"testing"
)

func TestGitRequestBody_GzipDecompresses(t *testing.T) {
	payload := []byte("hello git pack-protocol")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(payload); err != nil {
		t.Fatal(err)
	}
	gz.Close()

	req := httptest.NewRequest("POST", "/", &buf)
	req.Header.Set("Content-Encoding", "gzip")

	rc, err := gitRequestBody(req)
	if err != nil {
		t.Fatalf("gitRequestBody: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestGitRequestBody_PlainPassthrough(t *testing.T) {
	payload := []byte("plain body")
	req := httptest.NewRequest("POST", "/", bytes.NewReader(payload))

	rc, err := gitRequestBody(req)
	if err != nil {
		t.Fatalf("gitRequestBody: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}
